//go:build linux

package sandboxsupervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func validateTrustedPaths(config ServerConfig) error {
	if !filepath.IsAbs(config.SocketPath) || !filepath.IsAbs(config.ProfileStore) || !filepath.IsAbs(config.SandboxRoot) {
		return errors.New("socket, Profile store, and Sandbox root paths must be absolute")
	}
	return validateSocketDirectory(filepath.Dir(config.SocketPath))
}

func validateSocketDirectory(directoryPath string) error {
	if err := validateRuntimeOwnedRoot("socket directory", directoryPath); err != nil {
		return err
	}
	info, err := os.Lstat(directoryPath)
	if err != nil {
		return fmt.Errorf("inspect socket directory: %w", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o750 {
		return fmt.Errorf("socket directory permissions %04o must be 0750", permissions)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("socket directory has no Linux ownership metadata")
	}
	if int(stat.Gid) != os.Getegid() {
		return fmt.Errorf("socket directory group %d does not match Sandbox Supervisor GID %d", stat.Gid, os.Getegid())
	}
	return nil
}

func openRuntimeOwnedRoot(name, rootPath string) (*os.Root, error) {
	if err := validateRuntimeOwnedRoot(name, rootPath); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	return root, nil
}

func validateRuntimeOwnedRoot(name, rootPath string) error {
	info, err := os.Lstat(rootPath)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", name)
	}
	if permissions := info.Mode().Perm(); permissions&0o022 != 0 {
		return fmt.Errorf("%s permissions %04o permit group or other writes", name, permissions)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has no Linux ownership metadata", name)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s owner %d does not match Sandbox Supervisor UID %d", name, stat.Uid, os.Geteuid())
	}
	return nil
}
