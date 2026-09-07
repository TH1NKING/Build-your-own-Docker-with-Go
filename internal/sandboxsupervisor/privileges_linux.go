//go:build linux

package sandboxsupervisor

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	prSetNoNewPrivs = 38
	prCapAmbient    = 47
	prCapClearAll   = 4
)

// Linux capability ABI v3 has two 32-bit words for each set.
type capabilityHeader struct {
	Version uint32
	PID     int32
}

type capabilityData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

func restrictInitCapabilityInheritance() error {
	// Retain Init's existing effective/permitted capabilities for child UID
	// setup and reaping. Dropping the bounding set prevents future execs from
	// acquiring capabilities; it does not revoke Init's current authority.
	for capability := uintptr(0); ; capability++ {
		_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, syscall.PR_CAPBSET_READ, capability, 0, 0, 0, 0)
		if errno == syscall.EINVAL {
			break // The kernel, rather than a hard-coded last capability, decides.
		}
		if errno != 0 {
			return fmt.Errorf("inspect Init capability bounding set: %w", errno)
		}
		if err := setPrivilege(syscall.PR_CAPBSET_DROP, capability); err != nil {
			return fmt.Errorf("drop Init capability inheritance: %w", err)
		}
	}
	header := capabilityHeader{Version: 0x20080522}
	var data [2]capabilityData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("read Init capabilities: %w", errno)
	}
	data[0].Inheritable, data[1].Inheritable = 0, 0
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("clear inheritable Init capabilities: %w", errno)
	}
	return setPrivilege(prCapAmbient, prCapClearAll)
}

func dropWorkloadPrivileges() error {
	header := capabilityHeader{Version: 0x20080522}
	var empty [2]capabilityData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&empty[0])), 0); errno != 0 {
		return fmt.Errorf("clear Workload capabilities: %w", errno)
	}
	if err := setPrivilege(prCapAmbient, prCapClearAll); err != nil {
		return err
	}
	return setPrivilege(prSetNoNewPrivs, 1)
}

func setPrivilege(option, value uintptr) error {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, option, value, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("apply Sandbox privilege restriction %d: %w", option, errno)
	}
	return nil
}
