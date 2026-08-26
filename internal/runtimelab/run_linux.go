//go:build linux

// Package runtimelab contains the explicitly non-production Linux container
// demonstration shared by the standard and legacy command entry points.
package runtimelab

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	bridgeName    = "br0"
	hostIP        = "10.200.1.1/24"
	defaultRootFS = "./rootfs"
	baseCgroup    = "/sys/fs/cgroup/mydocker"
)

var (
	memLimit    string
	cpuLimit    int
	useID       string
	containerIP string
	cmdArgs     []string
)

// Main runs the Runtime Lab command.
func Main() {
	if len(os.Args) > 1 && os.Args[1] == "child-mode" {
		runChild()
		return
	}

	flag.StringVar(&memLimit, "m", "", "Memory limit (e.g. 100M)")
	flag.IntVar(&cpuLimit, "c", 0, "CPU limit percent (e.g. 20)")
	flag.StringVar(&useID, "id", "", "Resume existing container ID")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: mydocker [-m 100M] [-c 20] <IP> [COMMAND...]")
		os.Exit(1)
	}
	containerIP = args[0]
	if len(args) > 1 {
		cmdArgs = args[1:]
	} else {
		cmdArgs = []string{"/bin/sh"}
	}

	if os.Geteuid() != 0 {
		fmt.Println("Error: Must run as root")
		os.Exit(1)
	}

	runParent()
}

func runParent() {
	contID := useID
	if contID == "" {
		contID = fmt.Sprintf("%x", time.Now().UnixNano())[:8]
		fmt.Printf("=== Allocating New Container ID: %s ===\n", contID)
	} else {
		fmt.Printf("=== Resuming Container ID: %s ===\n", contID)
	}

	baseDir, _ := os.Getwd()
	conDir := filepath.Join(baseDir, "containers", contID)
	upperDir := filepath.Join(conDir, "upper")
	workDir := filepath.Join(conDir, "work")
	mergedDir := filepath.Join(conDir, "merged")
	baseImage, _ := filepath.Abs(defaultRootFS)

	must(os.MkdirAll(upperDir, 0o755))
	must(os.MkdirAll(workDir, 0o755))
	must(os.MkdirAll(mergedDir, 0o755))

	if !isMounted(mergedDir) {
		fmt.Println("Mounting OverlayFS...")
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", baseImage, upperDir, workDir)
		must(syscall.Mount("overlay", mergedDir, "overlay", 0, opts))
	}

	args := []string{"child-mode", containerIP, mergedDir, contID}
	args = append(args, cmdArgs...)

	cmd := exec.Command("/proc/self/exe", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC,
	}

	must(cmd.Start())
	pid := cmd.Process.Pid
	fmt.Printf("Container started with Host PID: %d\n", pid)

	setupCgroups(contID, pid)
	setupNetwork(contID, pid)

	_ = cmd.Wait()
	cleanup(contID, mergedDir)
}

func runChild() {
	ip := os.Args[2]
	rootfs := os.Args[3]
	id := os.Args[4]
	userCmd := os.Args[5:]

	fmt.Printf("--- Container Ready (%s) ---\n", id)

	must(syscall.Sethostname([]byte("container-" + id)))
	must(syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""))
	must(syscall.Chdir(rootfs))

	mountTmpfs("dev", "tmpfs")
	mountProc("proc")
	mountSys("sys")

	waitForInterface()

	if !strings.Contains(ip, "/") {
		ip += "/24"
	}
	must(exec.Command("ip", "addr", "add", ip, "dev", "eth0").Run())
	must(exec.Command("ip", "link", "set", "eth0", "up").Run())
	must(exec.Command("ip", "link", "set", "lo", "up").Run())

	gateway := strings.Split(hostIP, "/")[0]
	must(exec.Command("ip", "route", "add", "default", "via", gateway).Run())

	must(syscall.Chroot("."))
	must(syscall.Chdir("/"))

	env := []string{"PS1=[go-docker] # ", "PATH=/bin:/usr/bin:/sbin:/usr/sbin"}
	cmdPath, err := exec.LookPath(userCmd[0])
	if err != nil {
		cmdPath = userCmd[0]
	}

	must(syscall.Exec(cmdPath, userCmd, env))
}

func setupCgroups(id string, pid int) {
	cgDir := filepath.Join(baseCgroup, id)
	_ = os.MkdirAll(baseCgroup, 0o755)
	_ = os.WriteFile(filepath.Join(baseCgroup, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644)

	must(os.MkdirAll(cgDir, 0o755))

	if memLimit != "" {
		fmt.Printf("Limit Memory: %s\n", memLimit)
		must(os.WriteFile(filepath.Join(cgDir, "memory.max"), []byte(memLimit), 0o644))
		_ = os.WriteFile(filepath.Join(cgDir, "memory.swap.max"), []byte("0"), 0o644)
	}

	if cpuLimit > 0 {
		fmt.Printf("Limit CPU: %d%%\n", cpuLimit)
		quota := cpuLimit * 1000
		limitStr := fmt.Sprintf("%d 100000", quota)
		must(os.WriteFile(filepath.Join(cgDir, "cpu.max"), []byte(limitStr), 0o644))
	}

	must(os.WriteFile(filepath.Join(cgDir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644))
}

func setupNetwork(id string, pid int) {
	vethHost := "vethh-" + id
	vethCont := "vethc-" + id

	_ = exec.Command("ip", "link", "add", bridgeName, "type", "bridge").Run()
	_ = exec.Command("ip", "link", "set", bridgeName, "up").Run()
	_ = exec.Command("ip", "addr", "add", hostIP, "dev", bridgeName).Run()
	_ = exec.Command("ip", "link", "del", vethHost).Run()

	must(exec.Command("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethCont).Run())
	must(exec.Command("ip", "link", "set", vethHost, "master", bridgeName).Run())
	must(exec.Command("ip", "link", "set", vethHost, "up").Run())
	must(exec.Command("ip", "link", "set", vethCont, "netns", strconv.Itoa(pid)).Run())
}

func cleanup(id, mergedDir string) {
	fmt.Println()
	fmt.Println("Cleaning up...")
	_ = exec.Command("ip", "link", "del", "vethh-"+id).Run()
	_ = syscall.Unmount(mergedDir, syscall.MNT_DETACH)
	_ = os.Remove(filepath.Join(baseCgroup, id))
}

func waitForInterface() {
	for range 50 {
		ifaces, _ := os.ReadDir("sys/class/net")

		for _, iface := range ifaces {
			if strings.HasPrefix(iface.Name(), "vethc-") {
				_ = exec.Command("ip", "link", "set", iface.Name(), "name", "eth0").Run()
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("Timeout waiting for network interface")
}

func mountProc(target string) {
	_ = os.MkdirAll(target, 0o755)
	_ = syscall.Mount("proc", target, "proc", 0, "")
}

func mountSys(target string) {
	_ = os.MkdirAll(target, 0o755)
	_ = syscall.Mount("sysfs", target, "sysfs", 0, "")
}

func mountTmpfs(target, name string) {
	_ = os.MkdirAll(target, 0o755)
	_ = syscall.Mount(name, target, "tmpfs", 0, "")
}

func isMounted(dir string) bool {
	cmd := exec.Command("mountpoint", "-q", dir)
	return cmd.Run() == nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
