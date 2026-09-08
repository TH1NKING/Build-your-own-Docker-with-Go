//go:build linux

package sandboxsupervisor

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// RunBootstrap is the private re-exec entry point of sandboxd. It accepts no
// paths or commands: the Supervisor supplies already-open handles and storage policy.
func RunBootstrap(storagePolicy string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("Sandbox bootstrap requires namespace PID 1 and internal UID zero")
	}
	var storage StorageBudget
	if err := decodeStrictJSON([]byte(storagePolicy), &storage, "workspace_bytes", "workspace_files", "temporary_bytes", "temporary_files"); err != nil {
		return fmt.Errorf("decode Sandbox storage policy: %w", err)
	}
	if err := storage.validate(); err != nil {
		return err
	}
	ready := os.NewFile(3, "bootstrap-ready")
	control := os.NewFile(4, "init-requests")
	profile := os.NewFile(5, "profile-root")
	responses := os.NewFile(6, "init-responses")
	defer ready.Close()
	defer control.Close()
	defer responses.Close()
	defer profile.Close()
	for _, file := range []*os.File{ready, control, responses} {
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
	// User-namespace proc mounts need an existing fully visible proc instance.
	// Mount the new PID namespace's proc before detaching the inherited root;
	// afterwards the old proc is gone and kernels enforcing that check deny it.
	// This is a fresh proc mount, never a bind of the host's process view.
	if err := syscall.Mount("proc", newRoot+"/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount namespace-local process information: %w", err)
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
	// These empty mountpoints are reserved and materialized by the installer.
	// Their contents exist only in this Sandbox's private mount namespace.
	// Root, input and output consume three trusted inodes. Everything created
	// by a Workload, including directories and links, uses the remaining slots.
	if err := syscall.Mount("tmpfs", "/workspace", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, fmt.Sprintf("size=%d,nr_inodes=%d,mode=0755", storage.WorkspaceBytes, storage.WorkspaceFiles+3)); err != nil {
		return fmt.Errorf("mount Sandbox Workspace: %w", err)
	}
	// Reserve an immutable input slot. Authorized Attachment mounts are added
	// by their own feature; Workloads cannot write, replace or chmod this slot.
	if err := os.Mkdir("/workspace/input", 0o555); err != nil {
		return err
	}
	if err := os.Mkdir("/workspace/output", 0o700); err != nil {
		return err
	}
	if err := os.Chown("/workspace/output", 1000, 1000); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, fmt.Sprintf("size=%d,nr_inodes=%d,mode=0700,uid=1000,gid=1000", storage.TemporaryBytes, storage.TemporaryFiles+1)); err != nil {
		return fmt.Errorf("mount Sandbox temporary storage: %w", err)
	}
	// exec preserves namespace PID 1 while replacing the bootstrap with the
	// independently digest-verified Profile Bundle Init. It signals readiness.
	return syscall.Exec("/sandbox-init", []string{"sandbox-init"}, os.Environ())
}
