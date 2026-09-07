//go:build linux

package sandboxsupervisor

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type cgroup struct {
	parent    *os.Root
	group     *os.Root
	name      string
	directory os.FileInfo
}

// The budget group survives every Execution, including memory charges for
// Workspace tmpfs pages whose original Execution cgroup has been removed.
// Init lives in a sibling leaf so killing an Execution cannot kill PID 1.
type sandboxCgroups struct {
	budget    *cgroup
	init      *cgroup
	execution *cgroup
}

func newSandboxCgroups(directory string, budget ResourceBudget) (*sandboxCgroups, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("cgroup root must be absolute")
	}
	parent, err := openRuntimeOwnedRoot("Execution cgroup root", directory)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	var filesystem syscall.Statfs_t
	err = syscall.Fstatfs(int(file.Fd()), &filesystem)
	file.Close()
	if err != nil || filesystem.Type != 0x63677270 {
		return nil, errors.New("Sandbox requires writable cgroup v2")
	}
	if err := enableCgroupControllers(parent); err != nil {
		return nil, err
	}
	tree := &sandboxCgroups{}
	tree.budget, err = newCgroup(parent)
	if err != nil {
		return tree, err
	}
	for _, setting := range []struct{ name, value string }{
		{"cpu.max", strconv.FormatInt(budget.CPUMillis*100, 10) + " 100000"},
		{"memory.max", strconv.FormatInt(budget.MemoryBytes, 10)},
		{"memory.swap.max", strconv.FormatInt(budget.SwapBytes, 10)},
		{"pids.max", strconv.FormatInt(budget.PIDs, 10)},
	} {
		if err := tree.budget.set(setting.name, setting.value); err != nil {
			return tree, err
		}
	}
	if err := enableCgroupControllers(tree.budget.group); err != nil {
		return tree, err
	}
	tree.init, err = newCgroup(tree.budget.group)
	return tree, err
}

func enableCgroupControllers(group *os.Root) error {
	contents, err := group.ReadFile("cgroup.controllers")
	if err != nil {
		return err
	}
	available := " " + strings.Join(strings.Fields(string(contents)), " ") + " "
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(available, " "+controller+" ") {
			return fmt.Errorf("Sandbox requires delegated cgroup v2 %s controller", controller)
		}
	}
	return writeCgroupValue(group, "cgroup.subtree_control", "+cpu +memory +pids")
}

func newCgroup(root *os.Root) (*cgroup, error) {
	parent, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	name := "sandboxd-" + strconv.Itoa(os.Getpid()) + "-" + rand.Text()
	if err := parent.Mkdir(name, 0o700); err != nil {
		parent.Close()
		return nil, err
	}
	created := &cgroup{parent: parent, name: name}
	created.directory, err = parent.Lstat(name)
	if err != nil {
		return created, err
	}
	created.group, err = parent.OpenRoot(name)
	if err == nil {
		// Check support without issuing a kill before CLONE_INTO_CGROUP.
		// A kill changes the group's fork/kill sequence even while empty;
		// kernels may then kill the first process cloned directly into it.
		var file *os.File
		file, err = created.group.OpenFile("cgroup.kill", os.O_WRONLY|syscall.O_NOFOLLOW, 0)
		if err == nil {
			err = file.Close()
		}
	}
	// Return ownership even on partial creation, so the Sandbox rollback can
	// retain and retry failed cleanup rather than abandoning a kernel object.
	return created, err
}

func writeCgroupValue(group *os.Root, name, value string) error {
	file, err := group.OpenFile(name, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(value)
	return err
}

func (group *cgroup) set(name, value string) error {
	if err := writeCgroupValue(group.group, name, value); err != nil {
		return fmt.Errorf("set %s: %w", name, err)
	}
	actual, err := group.group.ReadFile(name)
	if err != nil || strings.Join(strings.Fields(string(actual)), " ") != value {
		return fmt.Errorf("cgroup did not confirm %s=%s (read error: %v)", name, value, err)
	}
	return nil
}

func (group *cgroup) attach(pid int) error {
	return writeCgroupValue(group.group, "cgroup.procs", strconv.Itoa(pid))
}

func (group *cgroup) killAndWait() error {
	if err := writeCgroupValue(group.group, "cgroup.kill", "1"); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		contents, err := group.group.ReadFile("cgroup.events")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(contents), "\n") {
			if line == "populated 0" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("Execution descendants did not leave their cgroup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (group *cgroup) close() error {
	if group == nil || group.parent == nil {
		return nil
	}
	current, err := group.parent.Lstat(group.name)
	if err != nil || group.directory == nil || !current.IsDir() || !os.SameFile(current, group.directory) {
		return errors.New("owned Sandbox cgroup directory was removed or replaced")
	}
	if group.group != nil {
		if err := group.killAndWait(); err != nil {
			return err
		}
	}
	if err := group.parent.Remove(group.name); err != nil {
		return fmt.Errorf("remove Sandbox cgroup: %w", err)
	}
	if group.group != nil {
		group.group.Close()
	}
	group.parent.Close()
	group.parent = nil
	return nil
}

func (tree *sandboxCgroups) close() error {
	if tree == nil {
		return nil
	}
	for _, group := range []*cgroup{tree.execution, tree.init, tree.budget} {
		if err := group.close(); err != nil {
			return err
		}
	}
	return nil
}

func (group *cgroup) resourceEvents() (ExecutionResourceUsage, error) {
	memory, err := group.counters("memory.events", "oom")
	if err != nil {
		return ExecutionResourceUsage{}, err
	}
	pids, err := group.counters("pids.events", "max")
	return ExecutionResourceUsage{OOMEvents: memory["oom"], PIDLimitEvents: pids["max"]}, err
}

func (group *cgroup) counters(name string, keys ...string) (map[string]uint64, error) {
	contents, err := group.group.ReadFile(name)
	if err != nil {
		return nil, err
	}
	values := make(map[string]uint64, len(keys))
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		for _, key := range keys {
			if len(fields) != 2 || fields[0] != key {
				continue
			}
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return nil, err
			}
			values[key] = value
		}
	}
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			return nil, fmt.Errorf("cgroup %s has no %s counter", name, key)
		}
	}
	return values, nil
}

func (group *cgroup) resourceUsage() (ExecutionResourceUsage, error) {
	usage, err := group.resourceEvents()
	if err != nil {
		return ExecutionResourceUsage{}, err
	}
	cpu, err := group.counters("cpu.stat", "usage_usec", "nr_throttled", "throttled_usec")
	if err != nil {
		return ExecutionResourceUsage{}, err
	}
	usage.CPUUsec = cpu["usage_usec"]
	usage.CPUThrottledPeriods = cpu["nr_throttled"]
	usage.CPUThrottledUsec = cpu["throttled_usec"]
	return usage, nil
}

func (after ExecutionResourceUsage) since(before ExecutionResourceUsage) (ExecutionResourceUsage, error) {
	if after.OOMEvents < before.OOMEvents || after.PIDLimitEvents < before.PIDLimitEvents || after.CPUUsec < before.CPUUsec || after.CPUThrottledPeriods < before.CPUThrottledPeriods || after.CPUThrottledUsec < before.CPUThrottledUsec {
		return ExecutionResourceUsage{}, errors.New("cgroup resource counters moved backwards")
	}
	return ExecutionResourceUsage{
		OOMEvents: after.OOMEvents - before.OOMEvents, CPUUsec: after.CPUUsec - before.CPUUsec,
		PIDLimitEvents:      after.PIDLimitEvents - before.PIDLimitEvents,
		CPUThrottledPeriods: after.CPUThrottledPeriods - before.CPUThrottledPeriods,
		CPUThrottledUsec:    after.CPUThrottledUsec - before.CPUThrottledUsec,
	}, nil
}
