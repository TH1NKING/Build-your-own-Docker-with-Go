//go:build linux

package workercredential

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
)

// LoadFile opens a credential without following a symbolic link, then checks the
// descriptor itself to avoid a pathname check/open race. Special files never
// reach the read step; O_NONBLOCK also prevents opening a FIFO from hanging.
func LoadFile(path string) (string, error) {
	unsafe := errors.New("Worker Credential file must be a private regular file owned by the Worker account")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", unsafe
	}
	file := os.NewFile(uintptr(fd), "worker-credential")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", unsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return "", unsafe
	}
	contents, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil || len(contents) > 128 {
		return "", errors.New("Worker Credential file content is invalid")
	}
	token := strings.TrimSpace(string(contents))
	if !validToken(token) {
		return "", errors.New("Worker Credential file content is invalid")
	}
	return token, nil
}
