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

type executionCgroup struct {
	parent *os.Root
	group  *os.Root
	name   string
}

func newExecutionCgroup(directory string) (*executionCgroup, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("cgroup root must be absolute")
	}
	parent, err := openRuntimeOwnedRoot("Execution cgroup root", directory)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			parent.Close()
		}
	}()
	file, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	var filesystem syscall.Statfs_t
	err = syscall.Fstatfs(int(file.Fd()), &filesystem)
	file.Close()
	if err != nil || filesystem.Type != 0x63677270 {
		return nil, errors.New("Execution requires writable cgroup v2")
	}
	name := "sandboxd-" + strconv.Itoa(os.Getpid()) + "-" + rand.Text()
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, err
	}
	group, err := parent.OpenRoot(name)
	if err != nil {
		parent.Remove(name)
		return nil, err
	}
	transferred = true
	execution := &executionCgroup{parent: parent, group: group, name: name}
	// Validate termination support on the empty cgroup before starting even
	// the trusted launcher. A v2 mount alone does not guarantee cgroup.kill.
	if err := execution.killAndWait(); err != nil {
		_ = execution.close()
		return nil, err
	}
	return execution, nil
}

func (group *executionCgroup) write(name, value string) error {
	file, err := group.group.OpenFile(name, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(value)
	return err
}

func (group *executionCgroup) attach(pid int) error {
	return group.write("cgroup.procs", strconv.Itoa(pid))
}

func (group *executionCgroup) killAndWait() error {
	if err := group.write("cgroup.kill", "1"); err != nil {
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

func (group *executionCgroup) close() error {
	defer group.parent.Close()
	err := group.killAndWait()
	group.group.Close()
	if removeErr := group.parent.Remove(group.name); removeErr != nil {
		return fmt.Errorf("remove Execution cgroup: %w", removeErr)
	}
	return err
}
