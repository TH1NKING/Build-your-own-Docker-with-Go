//go:build linux

package sandboxsupervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const sandboxIdentityCount = 65536

// ExtraFiles is not an inheritance whitelist: Go preserves a launcher's
// descriptors that lack CLOEXEC. Seal them once before serving. Marking instead
// of closing also keeps the Supervisor's own runtime descriptors valid. exec
// will explicitly clear CLOEXEC on the selected private pipes/Profile handles.
func sealInheritedDescriptors() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("inspect inherited Supervisor descriptors: %w", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC)
		if errno != 0 && errno != syscall.EBADF {
			return fmt.Errorf("seal inherited Supervisor descriptor: %w", errno)
		}
	}
	return nil
}

func validateSubordinateIDs(config ServerConfig) error {
	if config.SubUIDStart == 0 && config.SubGIDStart == 0 && config.SubIDCount == 0 {
		return nil // Preserve the unprivileged protocol-only mode.
	}
	if os.Geteuid() != 0 {
		return errors.New("Sandbox creation requires a root Supervisor")
	}
	if config.SubUIDStart == 0 || config.SubGIDStart == 0 || config.SubIDCount == 0 || config.SubIDCount%sandboxIdentityCount != 0 {
		return errors.New("subordinate UID/GID starts must be nonzero and count must be a positive multiple of 65536")
	}
	// UID/GID -1 is reserved by the kernel. Check without overflowing uint.
	const maximumID = uint64(1<<32 - 2)
	for _, start := range []uint{config.SubUIDStart, config.SubGIDStart} {
		if uint64(start) > maximumID || uint64(config.SubIDCount)-1 > maximumID-uint64(start) {
			return errors.New("subordinate ID range exceeds Linux UID/GID bounds")
		}
	}
	return nil
}

// Open each directory without following links. Handles keep trusted roots
// separate from caller identifiers throughout privileged path resolution.
func openInstalledProfileDirectory(store *os.Root, digest string) (*os.File, error) {
	const flags = os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	shard, err := store.OpenFile("sha256", flags, 0)
	if err != nil {
		return nil, err
	}
	defer shard.Close()
	if err := validateInstalledDirectory(shard, 0o755); err != nil {
		return nil, err
	}
	profileFD, err := syscall.Openat(int(shard.Fd()), digest, flags, 0)
	if err != nil {
		return nil, err
	}
	profile := os.NewFile(uintptr(profileFD), "installed-profile")
	if err := validateInstalledDirectory(profile, 0o555); err != nil {
		profile.Close()
		return nil, err
	}
	return profile, nil
}

func openProfileRootFilesystem(store *os.Root, digest string) (*os.File, error) {
	profile, err := openInstalledProfileDirectory(store, digest)
	if err != nil {
		return nil, err
	}
	defer profile.Close()
	const flags = os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	rootFD, err := syscall.Openat(int(profile.Fd()), "rootfs", flags, 0)
	if err != nil {
		return nil, err
	}
	rootfs := os.NewFile(uintptr(rootFD), "profile-rootfs")
	if err := validateInstalledDirectory(rootfs, 0o555); err != nil {
		rootfs.Close()
		return nil, err
	}
	return rootfs, nil
}

func validateInstalledDirectory(directory *os.File, mode fs.FileMode) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode().Perm() != mode {
		return fmt.Errorf("installed Profile directory must be root-owned with mode %04o", mode)
	}
	return nil
}

type sandboxCreator struct {
	ctx     context.Context
	cancel  context.CancelFunc
	config  ServerConfig
	root    *os.Root
	mu      sync.Mutex
	active  map[string]*createdSandbox
	retired map[string]struct{}
	wait    sync.WaitGroup
}

type createdSandbox struct {
	ctx            context.Context
	cancel         context.CancelFunc
	identityOffset uint
	directory      os.FileInfo
	execution      sync.Mutex
	process        *os.Process
	requests       *os.File
	responses      *os.File
	done           chan struct{}
	exited         chan struct{}
	terminating    bool  // guarded by creator.mu
	cleanupErr     error // guarded by creator.mu; retryable after done closes
}

func newSandboxCreator(ctx context.Context, config ServerConfig, root *os.Root) *sandboxCreator {
	ctx, cancel := context.WithCancel(ctx)
	return &sandboxCreator{ctx: ctx, cancel: cancel, config: config, root: root, active: make(map[string]*createdSandbox), retired: make(map[string]struct{})}
}

func (creator *sandboxCreator) enabled() bool { return creator.config.SubIDCount != 0 }

func (creator *sandboxCreator) close() {
	creator.cancel()
	creator.wait.Wait()
}

func (creator *sandboxCreator) reserve(id string) (*createdSandbox, ErrorCode) {
	creator.mu.Lock()
	defer creator.mu.Unlock()
	if creator.ctx.Err() != nil {
		return nil, ErrorCodeCreationFailed
	}
	if _, exists := creator.active[id]; exists {
		return nil, ErrorCodeSandboxExists
	}
	if _, exists := creator.retired[id]; exists {
		return nil, ErrorCodeSandboxExists
	}
	used := make(map[uint]bool, len(creator.active))
	for _, sandbox := range creator.active {
		used[sandbox.identityOffset] = true
	}
	var offset uint
	for offset < creator.config.SubIDCount && used[offset] {
		offset += sandboxIdentityCount
	}
	if offset == creator.config.SubIDCount {
		return nil, ErrorCodeIdentityRangeExhausted
	}
	if err := creator.root.Mkdir(id, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, ErrorCodeSandboxExists
		}
		return nil, ErrorCodeCreationFailed
	}
	ctx, cancel := context.WithCancel(creator.ctx)
	sandbox := &createdSandbox{ctx: ctx, cancel: cancel, identityOffset: offset, done: make(chan struct{}), exited: make(chan struct{})}
	sandbox.directory, _ = creator.root.Lstat(id)
	creator.active[id] = sandbox
	return sandbox, ""
}

func (creator *sandboxCreator) release(id string, sandbox *createdSandbox, retire bool) error {
	creator.mu.Lock()
	defer creator.mu.Unlock()
	return creator.releaseLocked(id, sandbox, retire)
}

func (creator *sandboxCreator) releaseLocked(id string, sandbox *createdSandbox, retire bool) error {
	// Only remove the empty directory this creation owns. Never recurse
	// into an unexpected tree or remove a pre-existing Sandbox identifier.
	info, err := creator.root.Lstat(id)
	if err == nil {
		if sandbox.directory == nil || !info.IsDir() || !os.SameFile(info, sandbox.directory) {
			err = errors.New("Sandbox runtime directory was replaced")
		} else {
			err = creator.root.Remove(id)
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	sandbox.cleanupErr = err
	if err != nil {
		// Keep the failed cleanup and identity reservation observable so a
		// retry cannot falsely acknowledge success or reuse uncertain state.
		fmt.Fprintln(os.Stderr, "Sandbox cleanup:", err)
		return err
	}
	delete(creator.active, id)
	if retire {
		creator.retired[id] = struct{}{}
	}
	return err
}

func (creator *sandboxCreator) create(operation *controlOperation, id string, profile *os.File) ErrorCode {
	sandbox, code := creator.reserve(id)
	if code != "" {
		return code
	}
	operation.own(sandbox)
	transferred := false
	defer func() {
		if !transferred {
			sandbox.cancel()
			creator.release(id, sandbox, false)
			close(sandbox.exited)
			close(sandbox.done)
		}
	}()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return ErrorCodeCreationFailed
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		return ErrorCodeCreationFailed
	}
	defer requestReader.Close()
	defer func() {
		if !transferred {
			requestWriter.Close()
		}
	}()
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		return ErrorCodeCreationFailed
	}
	defer responseWriter.Close()
	defer func() {
		if !transferred {
			responseReader.Close()
		}
	}()

	// Namespace construction happens before exec, so no goroutine in the
	// network-facing Supervisor ever changes its own namespaces or root.
	command := exec.CommandContext(sandbox.ctx, "/proc/self/exe", "--sandbox-bootstrap")
	// Go's container-aware GOMAXPROCS otherwise keeps host cgroup files open
	// across pivot_root. These trusted bootstrap settings close those handles
	// at runtime startup, before readiness. This is not a Workload CPU budget.
	command.Env = []string{"GOMAXPROCS=1", "GODEBUG=containermaxprocs=0,updatemaxprocs=0"}
	command.ExtraFiles = []*os.File{readyWriter, requestReader, profile, responseWriter}
	// Force an exec-managed pipe even when the Supervisor logs to a host file.
	command.Stderr = struct{ io.Writer }{os.Stderr}
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: int(creator.config.SubUIDStart + sandbox.identityOffset), Size: sandboxIdentityCount}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: int(creator.config.SubGIDStart + sandbox.identityOffset), Size: sandboxIdentityCount}},
		GidMappingsEnableSetgroups: true,
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{}},
	}
	if err := startFromProfile(command, profile); err != nil {
		fmt.Fprintln(os.Stderr, "Sandbox creation:", err)
		return ErrorCodeCreationFailed
	}
	readyWriter.Close()
	requestReader.Close()
	responseWriter.Close()
	_ = readyReader.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ready [1]byte
	if _, err := io.ReadFull(readyReader, ready[:]); err != nil || ready[0] != 'R' {
		_ = command.Process.Kill()
		_ = command.Wait()
		return ErrorCodeCreationFailed
	}
	transferred = true
	creator.mu.Lock()
	sandbox.process = command.Process
	sandbox.requests = requestWriter
	sandbox.responses = responseReader
	creator.mu.Unlock()
	creator.wait.Add(1)
	go func() {
		defer creator.wait.Done()
		_ = command.Wait()
		sandbox.cancel()
		creator.mu.Lock()
		sandbox.terminating = true
		creator.mu.Unlock()
		close(sandbox.exited)
		requestWriter.Close()
		responseReader.Close()
		// An in-flight Execution must finish using the inherited channels and
		// its existing cgroup cleanup before destruction can be acknowledged.
		sandbox.execution.Lock()
		defer sandbox.execution.Unlock()
		creator.release(id, sandbox, true)
		close(sandbox.done)
	}()
	return ""
}

func startFromProfile(command *exec.Cmd, profile *os.File) error {
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Do not unlock: Go retires this OS thread on return. CLONE_FS gives
		// only this thread its own cwd; other Supervisor threads keep theirs.
		if err := syscall.Unshare(syscall.CLONE_FS); err != nil {
			started <- err
			return
		}
		if err := syscall.Fchdir(int(profile.Fd())); err != nil {
			started <- err
			return
		}
		// Leave command.Dir empty. CLONE_NEWNS translates inherited cwd into
		// the new mount namespace, unlike an inherited directory FD.
		started <- command.Start()
	}()
	return <-started
}
