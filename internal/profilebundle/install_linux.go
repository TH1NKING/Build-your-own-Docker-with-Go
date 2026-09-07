//go:build linux

package profilebundle

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/systemcallpolicy"
)

const maximumBundleSize = 2 << 30

type pendingSymlink struct {
	name   string
	target string
}

type installedEntryFingerprint struct {
	name   string
	mode   fs.FileMode
	size   int64
	uid    uint32
	gid    uint32
	digest [sha256.Size]byte
	link   string
}

func Install(options InstallOptions) (installedPath string, returnErr error) {
	if os.Geteuid() != 0 {
		return "", errors.New("Profile Bundle installation requires root")
	}
	if options.BundlePath == "" || options.ExpectedSHA256 == "" || options.StorePath == "" {
		return "", errors.New("install requires --bundle, --expected-sha256, and --store")
	}
	if !filepath.IsAbs(options.StorePath) {
		return "", errors.New("Profile store path must be absolute")
	}
	expectedDigest, err := parseExpectedSHA256(options.ExpectedSHA256)
	if err != nil {
		return "", err
	}
	storeRoot, err := os.OpenRoot(options.StorePath)
	if err != nil {
		return "", fmt.Errorf("open Profile store: %w", err)
	}
	defer storeRoot.Close()
	if err := validateOpenedProfileStore(options.StorePath, storeRoot); err != nil {
		return "", err
	}
	if err := ensureStoreDirectory(storeRoot, ".staging", 0o700); err != nil {
		return "", err
	}
	if err := ensureStoreDirectory(storeRoot, "sha256", 0o755); err != nil {
		return "", err
	}

	stageName, err := randomStageName()
	if err != nil {
		return "", err
	}
	stageRelative := path.Join(".staging", stageName)
	if err := storeRoot.Mkdir(stageRelative, 0o700); err != nil {
		return "", fmt.Errorf("create installation staging directory: %w", err)
	}
	published := false
	defer func() {
		if published {
			return
		}
		if cleanupErr := storeRoot.RemoveAll(stageRelative); cleanupErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("clean installation staging directory: %w", cleanupErr)
		}
	}()

	stageRoot, err := storeRoot.OpenRoot(stageRelative)
	if err != nil {
		return "", fmt.Errorf("open installation staging directory: %w", err)
	}
	archive, actualDigest, err := copyApprovedBundle(stageRoot, options.BundlePath)
	if err != nil {
		stageRoot.Close()
		return "", err
	}
	if actualDigest != expectedDigest {
		archive.Close()
		stageRoot.Close()
		return "", fmt.Errorf("Profile Bundle SHA-256 is %s, want %s", actualDigest, expectedDigest)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		archive.Close()
		stageRoot.Close()
		return "", fmt.Errorf("rewind approved Profile Bundle: %w", err)
	}
	if err := extractApprovedBundle(stageRoot, archive); err != nil {
		archive.Close()
		stageRoot.Close()
		return "", err
	}
	if err := archive.Close(); err != nil {
		stageRoot.Close()
		return "", fmt.Errorf("close approved Profile Bundle: %w", err)
	}
	if err := stageRoot.Remove("bundle.tar"); err != nil {
		stageRoot.Close()
		return "", fmt.Errorf("remove staged transport archive: %w", err)
	}
	stageDirectory, err := stageRoot.Open(".")
	if err != nil {
		stageRoot.Close()
		return "", fmt.Errorf("open completed staging directory: %w", err)
	}
	if err := stageDirectory.Chmod(0o555); err != nil {
		stageDirectory.Close()
		stageRoot.Close()
		return "", fmt.Errorf("make completed staging directory immutable: %w", err)
	}
	if err := stageDirectory.Sync(); err != nil {
		stageDirectory.Close()
		stageRoot.Close()
		return "", fmt.Errorf("sync completed staging directory: %w", err)
	}
	if err := stageDirectory.Close(); err != nil {
		stageRoot.Close()
		return "", fmt.Errorf("close completed staging directory: %w", err)
	}
	if err := stageRoot.Close(); err != nil {
		return "", fmt.Errorf("close installation staging root: %w", err)
	}

	finalRelative := path.Join("sha256", expectedDigest)
	shaDirectory, err := storeRoot.Open("sha256")
	if err != nil {
		return "", fmt.Errorf("open Profile store identity directory: %w", err)
	}
	stagingDirectory, err := storeRoot.Open(".staging")
	if err != nil {
		shaDirectory.Close()
		return "", fmt.Errorf("open Profile store staging directory: %w", err)
	}
	if err := storeRoot.Rename(stageRelative, finalRelative); err != nil {
		shaDirectory.Close()
		stagingDirectory.Close()
		equal, comparisonErr := installedTreesEqual(storeRoot, stageRelative, finalRelative)
		if comparisonErr != nil || !equal {
			if comparisonErr != nil {
				return "", fmt.Errorf("publish installed Profile Bundle: %w; verify existing identity: %v", err, comparisonErr)
			}
			return "", fmt.Errorf("publish installed Profile Bundle: %w; existing identity has different contents", err)
		}
		return finalRelative, nil
	}
	if err := shaDirectory.Sync(); err != nil {
		shaDirectory.Close()
		stagingDirectory.Close()
		commitErr := fmt.Errorf("sync Profile store identity directory: %w", err)
		if rollbackErr := storeRoot.Rename(finalRelative, stageRelative); rollbackErr != nil {
			published = true
			return "", fmt.Errorf("%v; rollback published Profile: %w", commitErr, rollbackErr)
		}
		return "", commitErr
	}
	if err := stagingDirectory.Sync(); err != nil {
		shaDirectory.Close()
		stagingDirectory.Close()
		commitErr := fmt.Errorf("sync Profile store staging directory: %w", err)
		if rollbackErr := storeRoot.Rename(finalRelative, stageRelative); rollbackErr != nil {
			published = true
			return "", fmt.Errorf("%v; rollback published Profile: %w", commitErr, rollbackErr)
		}
		return "", commitErr
	}
	shaCloseErr := shaDirectory.Close()
	stagingCloseErr := stagingDirectory.Close()
	if shaCloseErr != nil || stagingCloseErr != nil {
		closeErr := errors.Join(shaCloseErr, stagingCloseErr)
		commitErr := fmt.Errorf("close synced Profile store directories: %w", closeErr)
		if rollbackErr := storeRoot.Rename(finalRelative, stageRelative); rollbackErr != nil {
			published = true
			return "", fmt.Errorf("%v; rollback published Profile: %w", commitErr, rollbackErr)
		}
		return "", commitErr
	}
	published = true
	return finalRelative, nil
}

func installedTreesEqual(storeRoot *os.Root, leftName, rightName string) (bool, error) {
	left, err := storeRoot.OpenRoot(leftName)
	if err != nil {
		return false, fmt.Errorf("open staged installed tree: %w", err)
	}
	defer left.Close()
	right, err := storeRoot.OpenRoot(rightName)
	if err != nil {
		return false, fmt.Errorf("open existing installed tree: %w", err)
	}
	defer right.Close()

	leftEntries, err := fingerprintInstalledTree(left)
	if err != nil {
		return false, fmt.Errorf("fingerprint staged installed tree: %w", err)
	}
	rightEntries, err := fingerprintInstalledTree(right)
	if err != nil {
		return false, fmt.Errorf("fingerprint existing installed tree: %w", err)
	}
	if len(leftEntries) != len(rightEntries) {
		return false, nil
	}
	for index := range leftEntries {
		if leftEntries[index] != rightEntries[index] {
			return false, nil
		}
	}
	return true, nil
}

func fingerprintInstalledTree(root *os.Root) ([]installedEntryFingerprint, error) {
	entries := make([]installedEntryFingerprint, 0)
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("entry %q has no Linux ownership metadata", name)
		}
		fingerprint := installedEntryFingerprint{
			name: name,
			mode: info.Mode(),
			size: info.Size(),
			uid:  stat.Uid,
			gid:  stat.Gid,
		}
		switch {
		case info.Mode().IsRegular():
			file, err := root.Open(name)
			if err != nil {
				return err
			}
			digest := sha256.New()
			if _, err := io.Copy(digest, file); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			copy(fingerprint.digest[:], digest.Sum(nil))
		case info.Mode()&os.ModeSymlink != 0:
			fingerprint.link, err = root.Readlink(name)
			if err != nil {
				return err
			}
		case info.IsDir():
		default:
			return fmt.Errorf("installed entry %q has unsupported type %v", name, info.Mode().Type())
		}
		entries = append(entries, fingerprint)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func parseExpectedSHA256(value string) (string, error) {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return "", errors.New("expected Profile Bundle SHA-256 must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("expected Profile Bundle SHA-256 must be 64 lowercase hexadecimal characters")
	}
	return value, nil
}

func validateOpenedProfileStore(storePath string, root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open Profile store directory handle: %w", err)
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		directory.Close()
		return fmt.Errorf("stat opened Profile store: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close Profile store directory handle: %w", err)
	}
	pathInfo, err := os.Lstat(storePath)
	if err != nil {
		return fmt.Errorf("lstat Profile store after opening: %w", err)
	}
	if !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.IsDir() {
		return errors.New("Profile store must be an existing directory, not a symbolic link")
	}
	openedStat, openedOK := openedInfo.Sys().(*syscall.Stat_t)
	pathStat, pathOK := pathInfo.Sys().(*syscall.Stat_t)
	if !openedOK || !pathOK || openedStat.Dev != pathStat.Dev || openedStat.Ino != pathStat.Ino {
		return errors.New("Profile store path changed while its directory handle was opened")
	}
	if openedStat.Uid != 0 || openedStat.Gid != 0 {
		return errors.New("Profile store must be owned by root:root")
	}
	if openedInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("Profile store must not be writable by group or other")
	}
	return nil
}

func ensureStoreDirectory(root *os.Root, name string, mode os.FileMode) error {
	if err := root.Mkdir(name, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Profile store directory %q: %w", name, err)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("stat Profile store directory %q: %w", name, err)
	}
	if !info.IsDir() || info.Mode().Perm() != mode {
		return fmt.Errorf("Profile store directory %q must be a directory with mode %04o", name, mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 0 {
		return fmt.Errorf("Profile store directory %q must be owned by root:root", name)
	}
	return nil
}

func randomStageName() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate installation staging name: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func copyApprovedBundle(stageRoot *os.Root, bundlePath string) (*os.File, string, error) {
	input, err := os.Open(bundlePath)
	if err != nil {
		return nil, "", fmt.Errorf("open Profile Bundle: %w", err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat Profile Bundle: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumBundleSize {
		return nil, "", fmt.Errorf("Profile Bundle must be a regular file between 1 and %d bytes", maximumBundleSize)
	}

	archive, err := stageRoot.OpenFile("bundle.tar", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return nil, "", fmt.Errorf("create staged Profile Bundle: %w", err)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(input, maximumBundleSize+1))
	if err != nil {
		archive.Close()
		return nil, "", fmt.Errorf("copy Profile Bundle to private staging: %w", err)
	}
	if written != info.Size() {
		archive.Close()
		return nil, "", errors.New("Profile Bundle changed while being copied to private staging")
	}
	if err := archive.Sync(); err != nil {
		archive.Close()
		return nil, "", fmt.Errorf("sync staged Profile Bundle: %w", err)
	}
	return archive, hex.EncodeToString(digest.Sum(nil)), nil
}

func extractApprovedBundle(stageRoot *os.Root, archive *os.File) error {
	archiveInfo, err := archive.Stat()
	if err != nil {
		return fmt.Errorf("stat approved Profile Bundle: %w", err)
	}
	reader := tar.NewReader(archive)
	manifestHeader, err := reader.Next()
	if err != nil {
		return fmt.Errorf("read Profile Bundle manifest header: %w", err)
	}
	if err := validateOuterHeader(manifestHeader, "manifest.json", 0o444, manifestHeader.Size); err != nil {
		return err
	}
	if manifestHeader.Size <= 0 || manifestHeader.Size > maximumPolicySize {
		return errors.New("Profile Bundle manifest has an invalid size")
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(reader, manifestHeader.Size+1))
	if err != nil {
		return fmt.Errorf("read Profile Bundle manifest: %w", err)
	}
	if int64(len(manifestBytes)) != manifestHeader.Size {
		return errors.New("Profile Bundle manifest size does not match its header")
	}
	bundleManifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return err
	}
	if err := writeInstalledBytes(stageRoot, "manifest.json", manifestBytes, 0o444); err != nil {
		return err
	}

	for _, declared := range bundleManifest.Components {
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf("read component %q header: %w", declared.Role, err)
		}
		mode := parseMode(declared.Mode)
		if err := validateOuterHeader(header, declared.Path, mode, declared.Size); err != nil {
			return err
		}
		file, err := stageRoot.OpenFile(declared.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create staged component %q: %w", declared.Role, err)
		}
		digest := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(file, digest), reader, declared.Size)
		if copyErr != nil {
			file.Close()
			return fmt.Errorf("read component %q: %w", declared.Role, copyErr)
		}
		if written != declared.Size || "sha256:"+hex.EncodeToString(digest.Sum(nil)) != declared.Digest {
			file.Close()
			return fmt.Errorf("component %q does not match its declared size and SHA-256", declared.Role)
		}
		if err := file.Chmod(os.FileMode(mode)); err != nil {
			file.Close()
			return fmt.Errorf("set component %q mode: %w", declared.Role, err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return fmt.Errorf("sync component %q: %w", declared.Role, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close component %q: %w", declared.Role, err)
		}
	}
	if header, err := reader.Next(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("Profile Bundle contains unlisted outer entry %q", header.Name)
		}
		return fmt.Errorf("finish Profile Bundle: %w", err)
	}
	if archiveInfo.Size() != canonicalOuterTarSize(manifestHeader.Size, bundleManifest.Components) {
		return errors.New("Profile Bundle has non-canonical framing or trailing data")
	}
	policyBytes, err := stageRoot.ReadFile("system-call-policy.json")
	if err != nil {
		return fmt.Errorf("read installed System Call Policy: %w", err)
	}
	canonicalPolicy, err := systemcallpolicy.Canonical(policyBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(policyBytes, canonicalPolicy) {
		return errors.New("Profile Bundle System Call Policy is not canonical JSON")
	}
	lockBytes, err := stageRoot.ReadFile("profile.lock.json")
	if err != nil {
		return fmt.Errorf("read installed profile build lock: %w", err)
	}
	lock, canonicalLock, err := decodeBuildLock(lockBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(lockBytes, canonicalLock) {
		return errors.New("Profile Bundle build lock is not canonical JSON")
	}
	if bundleManifest.Target != lock.Target || !pythonRuntimeEqual(bundleManifest.Python, lock.Python) {
		return errors.New("Profile Bundle manifest target and Python contract must exactly match the build lock")
	}
	return extractRootFS(stageRoot, lock.Python.Entrypoint)
}

func canonicalOuterTarSize(manifestSize int64, components []component) int64 {
	total := tarEntryStorageSize(manifestSize) + 2*512
	for _, declared := range components {
		total += tarEntryStorageSize(declared.Size)
	}
	return total
}

func tarEntryStorageSize(size int64) int64 {
	return 512 + ((size + 511) / 512 * 512)
}

func pythonRuntimeEqual(left, right pythonRuntime) bool {
	if left.Version != right.Version || left.Entrypoint != right.Entrypoint || len(left.Packages) != len(right.Packages) {
		return false
	}
	for index := range left.Packages {
		if left.Packages[index] != right.Packages[index] {
			return false
		}
	}
	return true
}

func decodeManifest(contents []byte) (manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var decoded manifest
	if err := decoder.Decode(&decoded); err != nil {
		return manifest{}, fmt.Errorf("decode Profile Bundle manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return manifest{}, fmt.Errorf("decode Profile Bundle manifest: %w", err)
	}
	if decoded.Schema != manifestSchema || decoded.Profile != profileID || decoded.Target.OS != "linux" || decoded.Target.Arch != "amd64" {
		return manifest{}, errors.New("Profile Bundle manifest selects an unsupported schema, profile, or target")
	}
	expected := bundleV1Components
	if len(decoded.Components) != len(expected) {
		return manifest{}, errors.New("Profile Bundle manifest must declare the closed v1 component set")
	}
	for index, want := range expected {
		got := decoded.Components[index]
		if got.Role != want.Role || got.Path != want.Path || got.Format != want.Format || got.Mode != want.Mode || got.Size <= 0 {
			return manifest{}, fmt.Errorf("Profile Bundle component %d does not match the closed v1 contract", index)
		}
		if _, err := parseDigest(got.Digest); err != nil {
			return manifest{}, fmt.Errorf("Profile Bundle component %q digest: %w", got.Role, err)
		}
	}
	canonical, err := canonicalJSON(decoded)
	if err != nil {
		return manifest{}, fmt.Errorf("encode canonical Profile Bundle manifest: %w", err)
	}
	if !bytes.Equal(contents, canonical) {
		return manifest{}, errors.New("Profile Bundle manifest is not canonical JSON")
	}
	return decoded, nil
}

func validateOuterHeader(header *tar.Header, name string, mode, size int64) error {
	if header.Name != name || header.Typeflag != tar.TypeReg || header.Mode != mode || header.Size != size ||
		header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
		header.Devmajor != 0 || header.Devminor != 0 || !header.ModTime.Equal(canonicalTime) ||
		!header.AccessTime.IsZero() || !header.ChangeTime.IsZero() || len(header.PAXRecords) != 0 ||
		header.Format != tar.FormatUSTAR {
		return fmt.Errorf("Profile Bundle entry %q has non-canonical metadata", header.Name)
	}
	return nil
}

func writeInstalledBytes(root *os.Root, name string, contents []byte, mode os.FileMode) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create installed %q: %w", name, err)
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return fmt.Errorf("write installed %q: %w", name, err)
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return fmt.Errorf("set installed %q mode: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync installed %q: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close installed %q: %w", name, err)
	}
	return nil
}

func extractRootFS(stageRoot *os.Root, entrypoint string) error {
	if err := stageRoot.Mkdir("rootfs", 0o700); err != nil {
		return fmt.Errorf("create installed root filesystem: %w", err)
	}
	root, err := stageRoot.OpenRoot("rootfs")
	if err != nil {
		return fmt.Errorf("open installed root filesystem: %w", err)
	}
	defer root.Close()
	component, err := stageRoot.Open("rootfs.tar")
	if err != nil {
		return fmt.Errorf("open verified root filesystem component: %w", err)
	}
	defer component.Close()
	originalDigest := sha256.New()
	if _, err := io.Copy(originalDigest, component); err != nil {
		return fmt.Errorf("hash verified root filesystem component: %w", err)
	}
	if _, err := component.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind verified root filesystem component: %w", err)
	}
	canonicalDigest := sha256.New()
	canonicalWriter := tar.NewWriter(canonicalDigest)

	reader := tar.NewReader(component)
	directories := map[string]struct{}{".": {}}
	entryTypes := make(map[string]byte)
	symlinks := make([]pendingSymlink, 0)
	lastName := ""
	itemCount := 0
	var unpackedBytes int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read verified root filesystem component: %w", err)
		}
		if err := validateRelativePath(header.Name); err != nil {
			return fmt.Errorf("root filesystem entry %q: %w", header.Name, err)
		}
		if lastName != "" && header.Name <= lastName {
			return errors.New("root filesystem entries must be unique and sorted")
		}
		lastName = header.Name
		itemCount++
		if itemCount > maximumRootFSItems {
			return fmt.Errorf("root filesystem exceeds the v1 limit of %d entries", maximumRootFSItems)
		}
		if header.Uid != 0 || header.Gid != 0 || !header.ModTime.Equal(canonicalTime) {
			return fmt.Errorf("root filesystem entry %q has non-canonical ownership or time", header.Name)
		}
		if err := ensureRootFSParents(root, path.Dir(header.Name), directories, entryTypes); err != nil {
			return err
		}
		if itemCount+len(directories)-1 > maximumRootFSItems {
			return fmt.Errorf("root filesystem files, links, and implicit directories exceed the v1 limit of %d entries", maximumRootFSItems)
		}

		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			if header.Typeflag != tar.TypeReg || header.Mode != 0o444 && header.Mode != 0o555 {
				return fmt.Errorf("root filesystem file %q has unsafe mode %04o", header.Name, header.Mode)
			}
			if header.Size < 0 || header.Size > maximumRootFSFile || unpackedBytes > maximumRootFSSize-header.Size {
				return fmt.Errorf("root filesystem file %q exceeds the v1 size budget", header.Name)
			}
			unpackedBytes += header.Size
			if err := canonicalWriter.WriteHeader(canonicalTarHeader(header.Name, tar.TypeReg, header.Mode, header.Size, "")); err != nil {
				return fmt.Errorf("canonicalize root filesystem file %q: %w", header.Name, err)
			}
			file, err := root.OpenFile(header.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create root filesystem file %q: %w", header.Name, err)
			}
			if _, err := io.CopyN(io.MultiWriter(file, canonicalWriter), reader, header.Size); err != nil {
				file.Close()
				return fmt.Errorf("write root filesystem file %q: %w", header.Name, err)
			}
			if err := file.Chmod(os.FileMode(header.Mode)); err != nil {
				file.Close()
				return fmt.Errorf("set root filesystem file %q mode: %w", header.Name, err)
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return fmt.Errorf("sync root filesystem file %q: %w", header.Name, err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close root filesystem file %q: %w", header.Name, err)
			}
			entryTypes[header.Name] = tar.TypeReg
		case tar.TypeSymlink:
			if header.Mode != 0o555 {
				return fmt.Errorf("root filesystem symlink %q has unsafe mode %04o", header.Name, header.Mode)
			}
			if err := validateSymlink(header.Name, header.Linkname); err != nil {
				return err
			}
			if err := canonicalWriter.WriteHeader(canonicalTarHeader(header.Name, tar.TypeSymlink, 0o555, 0, header.Linkname)); err != nil {
				return fmt.Errorf("canonicalize root filesystem symlink %q: %w", header.Name, err)
			}
			symlinks = append(symlinks, pendingSymlink{name: header.Name, target: header.Linkname})
			entryTypes[header.Name] = tar.TypeSymlink
		default:
			return fmt.Errorf("root filesystem entry %q has unsupported type %d", header.Name, header.Typeflag)
		}
	}
	if err := canonicalWriter.Close(); err != nil {
		return fmt.Errorf("finish canonical root filesystem: %w", err)
	}
	if !bytes.Equal(originalDigest.Sum(nil), canonicalDigest.Sum(nil)) {
		return errors.New("root filesystem component is not canonical rootfs-pax-tar/v1")
	}
	for _, symlink := range symlinks {
		if err := root.Symlink(symlink.target, symlink.name); err != nil {
			return fmt.Errorf("create root filesystem symlink %q: %w", symlink.name, err)
		}
	}
	if err := validateInstalledEntrypoint(root, entrypoint); err != nil {
		return err
	}
	// These paths belong to the installed Sandbox layout. Keep them separate
	// from rootfs.tar so runtime dependencies retain their independent digest.
	initBytes, err := stageRoot.ReadFile("sandbox-init")
	if err != nil {
		return fmt.Errorf("read verified Sandbox Init for installed root: %w", err)
	}
	if err := writeInstalledBytes(root, "sandbox-init", initBytes, 0o555); err != nil {
		return fmt.Errorf("materialize verified Sandbox Init in installed root: %w", err)
	}
	policyBytes, err := stageRoot.ReadFile("system-call-policy.json")
	if err != nil {
		return fmt.Errorf("read verified System Call Policy for installed root: %w", err)
	}
	if err := writeInstalledBytes(root, "system-call-policy.json", policyBytes, 0o444); err != nil {
		return fmt.Errorf("materialize verified System Call Policy in installed root: %w", err)
	}
	for _, name := range []string{"proc", "workspace", "tmp"} {
		if err := root.Mkdir(name, 0o700); err != nil {
			return fmt.Errorf("create reserved Sandbox mountpoint %q: %w", name, err)
		}
		directories[name] = struct{}{}
	}
	if err := makeDirectoriesReadOnly(root, directories); err != nil {
		return err
	}
	return nil
}

func validateInstalledEntrypoint(root *os.Root, entrypoint string) error {
	current := strings.TrimPrefix(entrypoint, "/")
	for range 40 {
		if err := validateRelativePath(current); err != nil {
			return fmt.Errorf("installed Python entrypoint %q is unsafe: %w", entrypoint, err)
		}
		info, err := root.Lstat(current)
		if err != nil {
			return fmt.Errorf("installed Python entrypoint %q is missing: %w", entrypoint, err)
		}
		if info.Mode().IsRegular() {
			if info.Mode().Perm() != 0o555 {
				return fmt.Errorf("installed Python entrypoint %q is not executable", entrypoint)
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("installed Python entrypoint %q does not resolve to a regular file", entrypoint)
		}
		target, err := root.Readlink(current)
		if err != nil {
			return fmt.Errorf("read installed Python entrypoint symlink %q: %w", current, err)
		}
		current = path.Clean(path.Join(path.Dir(current), target))
	}
	return fmt.Errorf("installed Python entrypoint %q has a symbolic-link cycle", entrypoint)
}

func ensureRootFSParents(root *os.Root, directory string, directories map[string]struct{}, entryTypes map[string]byte) error {
	if directory == "." {
		return nil
	}
	current := ""
	for _, segment := range strings.Split(directory, "/") {
		if current == "" {
			current = segment
		} else {
			current = path.Join(current, segment)
		}
		if entryTypes[current] == tar.TypeSymlink {
			return fmt.Errorf("root filesystem entry traverses symlink %q", current)
		}
		if _, exists := directories[current]; exists {
			continue
		}
		if err := root.Mkdir(current, 0o700); err != nil {
			return fmt.Errorf("create root filesystem directory %q: %w", current, err)
		}
		directories[current] = struct{}{}
	}
	return nil
}

func makeDirectoriesReadOnly(root *os.Root, directories map[string]struct{}) error {
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.Count(names[i], "/") > strings.Count(names[j], "/")
	})
	for _, name := range names {
		directory, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("open root filesystem directory %q: %w", name, err)
		}
		if err := directory.Chmod(0o555); err != nil {
			directory.Close()
			return fmt.Errorf("make root filesystem directory %q read-only: %w", name, err)
		}
		if err := directory.Sync(); err != nil {
			directory.Close()
			return fmt.Errorf("sync root filesystem directory %q: %w", name, err)
		}
		if err := directory.Close(); err != nil {
			return fmt.Errorf("close root filesystem directory %q: %w", name, err)
		}
	}
	return nil
}
