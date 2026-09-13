//go:build linux

package sandboxsupervisor

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// prepareSandboxDevices runs before pivot_root, in the new mount namespace.
// A fresh, bounded /dev contains exactly the two devices required by the
// noninteractive profile. Never bind the host device directory recursively.
func prepareSandboxDevices(newRoot string) error {
	const pathHandle = 0x200000 // Linux O_PATH; do not open/activate the device.
	directory, err := syscall.Open("/dev", pathHandle|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open trusted device directory: %w", err)
	}
	defer syscall.Close(directory)
	deviceRoot := newRoot + "/dev"
	if err := syscall.Mount("tmpfs", deviceRoot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=65536,nr_inodes=16,mode=0755"); err != nil {
		return fmt.Errorf("mount minimal Sandbox device directory: %w", err)
	}
	for _, device := range []struct {
		name  string
		minor uint64
	}{{"null", 3}, {"zero", 5}} {
		fd, err := syscall.Openat(directory, device.name, pathHandle|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open required Sandbox device %s: %w", device.name, err)
		}
		err = bindSandboxDevice(fd, deviceRoot+"/"+device.name, device.minor)
		_ = syscall.Close(fd)
		if err != nil {
			return err
		}
	}
	return makeKernelViewReadOnly(deviceRoot)
}

func bindSandboxDevice(fd int, target string, minor uint64) error {
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil {
		return fmt.Errorf("inspect required Sandbox device: %w", err)
	}
	// Both fixed device identities fit Linux's low 8-bit major/minor layout.
	// O_PATH|O_NOFOLLOW plus fstat rejects links and pins the checked object.
	if info.Mode&syscall.S_IFMT != syscall.S_IFCHR || info.Rdev != 1<<8|minor {
		return fmt.Errorf("required Sandbox device has an unexpected type or device identity")
	}
	placeholder, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create Sandbox device mountpoint: %w", err)
	}
	if err := placeholder.Close(); err != nil {
		return err
	}
	// Opened after NEWNS, the descriptor belongs to this mount namespace.
	// Inherited host mount descriptors cannot reliably serve as bind sources.
	source := "/proc/self/fd/" + strconv.Itoa(fd)
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind required Sandbox device: %w", err)
	}
	return makeKernelViewReadOnly(target)
}

// restrictSandboxProc runs after mounting this PID namespace's read-only proc,
// but before detaching the old root. Process status/fd/net remain available;
// fixed sensitive global interfaces are covered by empty read-only views.
// This is not a claim that all proc statistics are namespace-local: e.g.
// meminfo and uptime still reflect the shared kernel.
func restrictSandboxProc(newRoot string) error {
	for _, name := range []string{"sys", "irq", "bus", "fs", "acpi", "scsi"} {
		target := newRoot + "/proc/" + name
		info, err := os.Lstat(target)
		if os.IsNotExist(err) {
			continue // These entries vary with kernel configuration.
		}
		if err != nil || !info.IsDir() {
			return fmt.Errorf("inspect sensitive proc directory %s: unexpected type or %v", name, err)
		}
		if err := syscall.Mount("tmpfs", target, "tmpfs", syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=4096,nr_inodes=1,mode=0555"); err != nil {
			return fmt.Errorf("hide sensitive proc directory %s: %w", name, err)
		}
	}
	for _, name := range []string{"kcore", "keys", "timer_list", "sysrq-trigger", "interrupts", "kallsyms", "slabinfo", "vmallocinfo", "modules", "iomem", "ioports", "sched_debug"} {
		target := newRoot + "/proc/" + name
		info, err := os.Lstat(target)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("inspect sensitive proc file %s: unexpected type or %v", name, err)
		}
		if err := syscall.Mount(newRoot+"/dev/null", target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("hide sensitive proc file %s: %w", name, err)
		}
		if err := makeKernelViewReadOnly(target); err != nil {
			return err
		}
	}
	return nil
}

func makeKernelViewReadOnly(target string) error {
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(target, &filesystem); err != nil {
		return fmt.Errorf("inspect Sandbox kernel view mount flags: %w", err)
	}
	// Linux >= 3.17 preserves the existing atime policy when all atime flags
	// are omitted. In particular statfs ST_RELATIME is not mount MS_RELATIME.
	const preserved = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	flags := uintptr(filesystem.Flags)&preserved | syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NOEXEC
	if err := syscall.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("make Sandbox kernel view read-only: %w", err)
	}
	return nil
}
