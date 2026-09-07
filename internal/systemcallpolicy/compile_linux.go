//go:build linux

package systemcallpolicy

import (
	"errors"
	"runtime"
	"syscall"
)

// Compile produces a native linux/amd64 cBPF program; it never installs it or
// changes the calling thread. The launcher owns that irreversible transition.
func Compile(policy Policy) ([]syscall.SockFilter, error) {
	if runtime.GOARCH != "amd64" {
		return nil, errors.New("System Call Policy enforcement requires linux/amd64")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	program, err := compileInstructions(policy)
	if err != nil {
		return nil, err
	}
	filters := make([]syscall.SockFilter, len(program))
	for index, instruction := range program {
		filters[index] = syscall.SockFilter{Code: instruction.code, Jt: instruction.jt, Jf: instruction.jf, K: instruction.k}
	}
	return filters, nil
}
