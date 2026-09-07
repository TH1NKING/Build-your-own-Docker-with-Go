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

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/systemcallpolicy"
)

type installedProfileComponent struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	Format string `json:"format"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// The installed manifest binds both copies inside the root to their verified
// outer components. Each path is resolved through trusted directory handles.
func validateSandboxProfile(store *os.Root, digest string, rootfs *os.File) error {
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
		Schema     string                      `json:"schema"`
		Profile    string                      `json:"profile"`
		Components []installedProfileComponent `json:"components"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode installed Profile manifest: %w", err)
	}
	if manifest.Schema != "profile-bundle-manifest/v1" || manifest.Profile != "python-data-v1" {
		return errors.New("installed Profile manifest has an unsupported identity")
	}
	initFile, err := verifiedInstalledComponent(rootfs, manifest.Components, "sandbox-init", "sandbox-init", "opaque/v1", 0o555, 64<<20)
	if err != nil {
		return err
	}
	defer initFile.Close()
	policyFile, err := verifiedInstalledComponent(rootfs, manifest.Components, "system-call-policy", "system-call-policy.json", "system-call-policy/v1", 0o444, 1<<20)
	if err != nil {
		return err
	}
	defer policyFile.Close()
	policyBytes, err := io.ReadAll(io.LimitReader(policyFile, (1<<20)+1))
	if err != nil {
		return err
	}
	policy, err := systemcallpolicy.Parse(policyBytes)
	if err != nil {
		return err
	}
	// Reject unsupported policies before allocating Sandbox resources. The
	// launcher reads the same verified copy through the read-only Profile.
	_, err = systemcallpolicy.Compile(policy)
	return err
}

func verifiedInstalledComponent(rootfs *os.File, components []installedProfileComponent, role, name, format string, mode os.FileMode, maximumSize int64) (*os.File, error) {
	var expected *installedProfileComponent
	for index := range components {
		component := &components[index]
		if component.Role != role {
			continue
		}
		if expected != nil || component.Path != name || component.Format != format || component.Mode != fmt.Sprintf("%04o", mode) || component.Size <= 0 || component.Size > maximumSize {
			return nil, fmt.Errorf("installed Profile must declare one valid %s component", role)
		}
		expected = component
	}
	if expected == nil || !strings.HasPrefix(expected.Digest, "sha256:") || len(expected.Digest) != len("sha256:")+64 {
		return nil, fmt.Errorf("installed Profile must declare the %s SHA-256", role)
	}
	file, err := openInstalledRegularAt(int(rootfs.Fd()), name, mode, maximumSize)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, expected.Size+1))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("hash installed %s: %w", role, err)
	}
	if size != expected.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expected.Digest {
		file.Close()
		return nil, fmt.Errorf("installed %s differs from its verified Profile component", role)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
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
