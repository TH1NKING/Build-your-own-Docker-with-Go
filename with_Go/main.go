//go:build linux

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 配置常量
const (
	BridgeName    = "br0"
	HostIP        = "10.200.1.1/24"
	DefaultRootFS = "./rootfs"
	BaseCgroup    = "/sys/fs/cgroup/mydocker"
)

// 全局变量用于参数解析
var (
	memLimit    string
	cpuLimit    int
	useID       string
	containerIP string
	cmdArgs     []string
)

func main() {
	// 0. 特殊判断：如果是子进程模式，直接跳转到 child 逻辑
	if len(os.Args) > 1 && os.Args[1] == "child-mode" {
		runChild()
		return
	}

	// 1. 参数解析
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

	// 2. 检查 Root 权限
	if os.Geteuid() != 0 {
		fmt.Println("Error: Must run as root")
		os.Exit(1)
	}

	// 3. 运行父进程逻辑
	runParent()
}

// ==========================================
// Parent Logic (宿主机逻辑)
// ==========================================
func runParent() {
	// 生成或复用 ID
	contID := useID
	if contID == "" {
		contID = fmt.Sprintf("%x", time.Now().UnixNano())[:8]
		fmt.Printf("=== Allocating New Container ID: %s ===\n", contID)
	} else {
		fmt.Printf("=== Resuming Container ID: %s ===\n", contID)
	}

	// 准备目录
	baseDir, _ := os.Getwd()
	conDir := filepath.Join(baseDir, "containers", contID)
	upperDir := filepath.Join(conDir, "upper")
	workDir := filepath.Join(conDir, "work")
	mergedDir := filepath.Join(conDir, "merged")
	baseImage, _ := filepath.Abs(DefaultRootFS)

	must(os.MkdirAll(upperDir, 0755))
	must(os.MkdirAll(workDir, 0755))
	must(os.MkdirAll(mergedDir, 0755))

	// 挂载 OverlayFS
	// 对应 Bash: mount -t overlay ...
	if !isMounted(mergedDir) {
		fmt.Println("Mounting OverlayFS...")
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", baseImage, upperDir, workDir)
		must(syscall.Mount("overlay", mergedDir, "overlay", 0, opts))
	}

	// 准备启动子进程
	// 这里调用 /proc/self/exe 再次运行自己，但是传入 "child-mode"
	// 并带上 Namespace 隔离标志
	args := []string{"child-mode", containerIP, mergedDir, contID}
	args = append(args, cmdArgs...)

	cmd := exec.Command("/proc/self/exe", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 设置 Namespace (对应 unshare 的参数)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC,
	}

	// 启动子进程
	must(cmd.Start())
	pid := cmd.Process.Pid
	fmt.Printf("Container started with Host PID: %d\n", pid)

	// 配置 Cgroups (Worker 逻辑)
	setupCgroups(contID, pid)

	// 配置网络 (Worker 逻辑)
	setupNetwork(contID, pid)

	// 等待子进程退出
	_ = cmd.Wait()

	// 清理逻辑 (Trap cleanup)
	cleanup(contID, mergedDir)
}

// ==========================================
// Child Logic (容器内 Init 逻辑)
// ==========================================
func runChild() {
	// 解析参数
	// args[0]="child-mode", args[1]=IP, args[2]=RootFS, args[3]=ID, args[4:]=CMD
	ip := os.Args[2]
	rootfs := os.Args[3]
	id := os.Args[4]
	userCmd := os.Args[5:]

	fmt.Printf("--- Container Ready (%s) ---\n", id)

	// 1. 设置 Hostname
	must(syscall.Sethostname([]byte("container-" + id)))

	// 2. 挂载 /proc (关键！)
	// 先把根变成私有挂载，防止污染宿主机
	must(syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""))

	// Chroot 到 Merged 目录
	must(syscall.Chdir(rootfs))

	// 挂载必要的伪文件系统
	mountTmpfs("dev", "tmpfs")
	mountProc("proc") // 必须先挂载 proc 才能看网络
	mountSys("sys")

	// 3. 等待并配置网络
	// 简单的轮询等待 eth0 出现 (对应 Bash 中的循环等待)
	waitForInterface("eth0")

	// 配置 IP (这里偷懒调用 ip 命令，用 Go 的 netlink 库会更原生但代码太长)
	if !strings.Contains(ip, "/") {
		ip = ip + "/24"
	}
	must(exec.Command("ip", "addr", "add", ip, "dev", "eth0").Run())
	must(exec.Command("ip", "link", "set", "eth0", "up").Run())
	must(exec.Command("ip", "link", "set", "lo", "up").Run())

	// 设置默认网关
	gateway := strings.Split(HostIP, "/")[0]
	must(exec.Command("ip", "route", "add", "default", "via", gateway).Run())

	// 4. 切换 Root (Chroot)
	must(syscall.Chroot("."))
	must(syscall.Chdir("/"))

	// 5. 真正运行用户命令 (Exec)
	// 使用 syscall.Exec 替换当前进程，不再 fork
	env := []string{"PS1=[go-docker] # ", "PATH=/bin:/usr/bin:/sbin:/usr/sbin"}
	cmdPath, err := exec.LookPath(userCmd[0])
	if err != nil {
		cmdPath = userCmd[0] // 如果找不到，尝试直接用
	}

	must(syscall.Exec(cmdPath, userCmd, env))
}

// ==========================================
// Helpers (辅助函数)
// ==========================================

func setupCgroups(id string, pid int) {
	cgDir := filepath.Join(BaseCgroup, id)
	// 确保父目录开启 controller
	os.MkdirAll(BaseCgroup, 0755)
	_ = ioutil.WriteFile(filepath.Join(BaseCgroup, "cgroup.subtree_control"), []byte("+cpu +memory"), 0644)

	// 创建子 Cgroup
	must(os.MkdirAll(cgDir, 0755))

	// 限制 Memory
	if memLimit != "" {
		fmt.Printf("Limit Memory: %s\n", memLimit)
		must(ioutil.WriteFile(filepath.Join(cgDir, "memory.max"), []byte(memLimit), 0644))
		_ = ioutil.WriteFile(filepath.Join(cgDir, "memory.swap.max"), []byte("0"), 0644)
	}

	// 限制 CPU
	if cpuLimit > 0 {
		fmt.Printf("Limit CPU: %d%%\n", cpuLimit)
		quota := cpuLimit * 1000
		limitStr := fmt.Sprintf("%d 100000", quota)
		must(ioutil.WriteFile(filepath.Join(cgDir, "cpu.max"), []byte(limitStr), 0644))
	}

	// 加入进程
	must(ioutil.WriteFile(filepath.Join(cgDir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0644))
}

func setupNetwork(id string, pid int) {
	vethHost := "vethh-" + id
	vethCont := "vethc-" + id

	// 确保网桥
	exec.Command("ip", "link", "add", BridgeName, "type", "bridge").Run()
	exec.Command("ip", "link", "set", BridgeName, "up").Run()
	exec.Command("ip", "addr", "add", HostIP, "dev", BridgeName).Run()

	// 清理旧 veth
	exec.Command("ip", "link", "del", vethHost).Run()

	// 创建 Pair
	// ip link add vethh-id type veth peer name vethc-id
	must(exec.Command("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethCont).Run())

	// 挂载到网桥
	must(exec.Command("ip", "link", "set", vethHost, "master", BridgeName).Run())
	must(exec.Command("ip", "link", "set", vethHost, "up").Run())

	// 移动到容器
	// ip link set vethc-id netns pid
	must(exec.Command("ip", "link", "set", vethCont, "netns", strconv.Itoa(pid)).Run())

	// 在父进程中我们只能改名到这一步，改名成 eth0 的操作必须在子进程做
	// 但为了方便子进程识别，这里我们不做额外操作，子进程里直接找 vethc-*
}

func cleanup(id string, mergedDir string) {
	fmt.Println("\nCleaning up...")
	// 删除 veth
	exec.Command("ip", "link", "del", "vethh-"+id).Run()

	// 卸载 OverlayFS (尝试多次)
	// 懒卸载
	syscall.Unmount(mergedDir, syscall.MNT_DETACH)

	// [修复点] 使用 os.Remove 替代不存在的 os.RemoveDir
	os.Remove(filepath.Join(BaseCgroup, id))
}
func waitForInterface(namePrefix string) {
	// 简单的轮询，等待 veth 别移进来
	for i := 0; i < 50; i++ {
		ifaces, _ := os.ReadDir("/sys/class/net")
		for _, f := range ifaces {
			if strings.HasPrefix(f.Name(), "vethc-") {
				// 找到了！重命名为 eth0
				exec.Command("ip", "link", "set", f.Name(), "name", "eth0").Run()
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("Timeout waiting for network interface")
}

// 简单的挂载封装
func mountProc(target string) {
	os.MkdirAll(target, 0755)
	syscall.Mount("proc", target, "proc", 0, "")
}
func mountSys(target string) {
	os.MkdirAll(target, 0755)
	syscall.Mount("sysfs", target, "sysfs", 0, "")
}
func mountTmpfs(target, name string) {
	os.MkdirAll(target, 0755)
	syscall.Mount(name, target, "tmpfs", 0, "")
}

func isMounted(dir string) bool {
	// 简单检查是否挂载：查看 /proc/mounts 或者调用 mountpoint (这里简单处理)
	// 真正的实现可以读取 /proc/self/mountinfo
	cmd := exec.Command("mountpoint", "-q", dir)
	return cmd.Run() == nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
