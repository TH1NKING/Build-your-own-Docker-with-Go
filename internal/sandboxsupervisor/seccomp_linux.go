//go:build linux

package sandboxsupervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/systemcallpolicy"
)

func workloadSystemCallFilter() ([]syscall.SockFilter, error) {
	// The Supervisor verified this fixed copy against the manifest before
	// mounting the Profile read-only. Host UID 0 is unmapped inside the User
	// Namespace, so ownership validation belongs outside, in the Supervisor.
	file, err := os.OpenFile("/system-call-policy.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open Workload System Call Policy: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 || info.Size() <= 0 || info.Size() > 1<<20 {
		return nil, errors.New("Workload System Call Policy must be a bounded read-only regular file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || int64(len(contents)) != info.Size() {
		return nil, errors.New("cannot read complete Workload System Call Policy")
	}
	policy, err := systemcallpolicy.Parse(contents)
	if err != nil {
		return nil, err
	}
	return systemcallpolicy.Compile(policy)
}

func installWorkloadSystemCallFilter(filter []syscall.SockFilter) error {
	if len(filter) == 0 || len(filter) > 4096 {
		return errors.New("invalid Workload System Call Policy program size")
	}
	program := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	// The caller is locked to the thread that will exec Python. Other Go
	// threads run trusted code only and disappear at exec; no Workload runs
	// in this process before that boundary. Filtering Init itself would also
	// deny its privileged process-management operations.
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, syscall.PR_SET_SECCOMP, 2, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("install Workload System Call Policy: %w", errno)
	}
	return nil
}
