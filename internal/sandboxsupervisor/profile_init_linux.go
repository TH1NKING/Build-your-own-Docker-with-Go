//go:build linux

package sandboxsupervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// The installed manifest binds the executable inside the root to the verified
// outer Init component. Each path is resolved through trusted directory handles.
func validateSandboxInit(store *os.Root, digest string, rootfs *os.File) error {
	profile, err := openInstalledProfileDirectory(store, digest)
	if err != nil {
		return err
	}
	defer profile.Close()
	manifestFile, err := openInstalledRegularAt(int(profile.Fd()), "manifest.json", 0o444, 1<<20)
	if err != nil {
		return err
	}
	defer manifestFile.Close()
	manifestBytes, err := io.ReadAll(io.LimitReader(manifestFile, (1<<20)+1))
	if err != nil || len(manifestBytes) > 1<<20 {
		return errors.New("invalid installed Profile manifest size")
	}
	var manifest struct {
		Schema     string `json:"schema"`
		Profile    string `json:"profile"`
		Components []struct {
			Role   string `json:"role"`
			Path   string `json:"path"`
			Format string `json:"format"`
			Mode   string `json:"mode"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"components"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode installed Profile manifest: %w", err)
	}
	if manifest.Schema != "profile-bundle-manifest/v1" || manifest.Profile != "python-data-v1" {
		return errors.New("installed Profile manifest has an unsupported identity")
	}
	var expectedDigest string
	var expectedSize int64
	initFound := false
	for _, component := range manifest.Components {
		if component.Role != "sandbox-init" {
			continue
		}
		if initFound || component.Path != "sandbox-init" || component.Format != "opaque/v1" || component.Mode != "0555" || component.Size <= 0 || component.Size > 64<<20 {
			return errors.New("installed Profile must declare one executable Sandbox Init component")
		}
		initFound = true
		expectedDigest = component.Digest
		expectedSize = component.Size
	}
	if !strings.HasPrefix(expectedDigest, "sha256:") || len(expectedDigest) != len("sha256:")+64 {
		return errors.New("installed Profile must declare the Sandbox Init SHA-256")
	}
	initFile, err := openInstalledRegularAt(int(rootfs.Fd()), "sandbox-init", 0o555, 64<<20)
	if err != nil {
		return err
	}
	defer initFile.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(initFile, expectedSize+1))
	if err != nil {
		return fmt.Errorf("hash installed Sandbox Init: %w", err)
	}
	if size != expectedSize || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return errors.New("installed Sandbox Init differs from its verified Profile component")
	}
	return nil
}

func openInstalledRegularAt(directoryFD int, name string, mode os.FileMode, maximumSize int64) (*os.File, error) {
	fd, err := syscall.Openat(directoryFD, name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || info.Size() <= 0 || info.Size() > maximumSize {
		file.Close()
		return nil, fmt.Errorf("installed Profile file %q must be a bounded root-owned regular file with mode %04o", name, mode)
	}
	return file, nil
}
