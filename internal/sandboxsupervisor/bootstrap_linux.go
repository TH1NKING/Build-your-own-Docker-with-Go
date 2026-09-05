//go:build linux

package sandboxsupervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
)

// RunBootstrap is the private re-exec entry point of sandboxd. It accepts no
// paths or commands: the Supervisor supplies only already-open handles. T04
// keeps this trusted PID 1 alive; Workload execution is implemented by T05.
func RunBootstrap() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("Sandbox bootstrap requires namespace PID 1 and internal UID zero")
	}
	ready := os.NewFile(3, "bootstrap-ready")
	lifetime := os.NewFile(4, "bootstrap-lifetime")
	profile := os.NewFile(5, "profile-root")
	defer ready.Close()
	defer lifetime.Close()
	defer profile.Close()
	for _, file := range []*os.File{ready, lifetime} {
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("Sandbox bootstrap requires private inherited pipes")
		}
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make Sandbox mount propagation private: %w", err)
	}
	cwd, err := os.Stat(".")
	if err != nil {
		return err
	}
	selected, err := profile.Stat()
	if err != nil || !os.SameFile(cwd, selected) {
		return errors.New("bootstrap cwd is not the selected Profile")
	}
	// Build the mountpoint in this namespace. No host directory is made
	// writable or traversable by subordinate IDs, including 0700 ancestors.
	temporary, err := os.Lstat("/tmp")
	if err != nil || !temporary.IsDir() || temporary.Mode()&os.ModeSymlink != 0 {
		return errors.New("bootstrap requires a real /tmp directory")
	}
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "size=1m,nr_inodes=16,mode=0700"); err != nil {
		return fmt.Errorf("mount private bootstrap staging: %w", err)
	}
	const newRoot = "/tmp/rootfs"
	if err := os.Mkdir(newRoot, 0o755); err != nil {
		return err
	}
	// Non-recursive bind excludes nested host mounts. Cwd was translated by
	// CLONE_NEWNS; the inherited Profile FD still refers to the parent mount.
	if err := syscall.Mount(".", newRoot, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind Profile root filesystem: %w", err)
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(newRoot, &filesystem); err != nil {
		return fmt.Errorf("inspect Profile mount flags: %w", err)
	}
	const preserved = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_NOATIME | syscall.MS_NODIRATIME | syscall.MS_RELATIME | syscall.MS_SYNCHRONOUS | syscall.MS_MANDLOCK
	flags := uintptr(filesystem.Flags)&preserved | syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV
	if err := syscall.Mount("", newRoot, "", flags, ""); err != nil {
		return fmt.Errorf("make Profile mount read-only: %w", err)
	}
	if err := syscall.Chdir(newRoot); err != nil {
		return fmt.Errorf("enter Profile mount: %w", err)
	}
	// Stack the old root on the new one, then detach it. This is the documented
	// pivot_root(".", ".") idiom; it needs no writable put_old directory.
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot Sandbox root: %w", err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old Sandbox root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("reset Sandbox working directory: %w", err)
	}
	if err := profile.Close(); err != nil {
		return err
	}
	if _, err := ready.Write([]byte{'R'}); err != nil {
		return err
	}
	ready.Close()
	// EOF also ends this trusted bootstrap if the Supervisor dies. No caller
	// command or Workload can enter through this lifetime-only channel.
	_, err = io.Copy(io.Discard, lifetime)
	return err
}
