package profilebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	buildLockSchema    = "profile-build-lock/v1"
	manifestSchema     = "profile-bundle-manifest/v1"
	profileID          = "python-data-v1"
	maximumInputSize   = 256 << 20
	maximumInitSize    = 64 << 20
	maximumPolicySize  = 1 << 20
	maximumRootFSSize  = 1 << 30
	maximumRootFSFile  = 256 << 20
	maximumRootFSItems = 100000
	maximumPathLength  = 4096
)

var canonicalTime = time.Unix(0, 0).UTC()

type bundlePart struct {
	component component
	contents  []byte
	filePath  string
}

type rootfsBuildEntry struct {
	name     string
	typeFlag byte
	mode     int64
	size     int64
	linkName string
	spool    string
}

type rootfsBuildBudget struct {
	scannedItems int
	scannedBytes int64
	outputItems  int
	outputBytes  int64
}

func Build(options BuildOptions) (string, error) {
	if err := validateBuildOptions(options); err != nil {
		return "", err
	}

	lock, lockBytes, err := readBuildLock(options.LockPath)
	if err != nil {
		return "", err
	}
	policyBytes, err := readSystemCallPolicy(options.SystemCallPolicyPath)
	if err != nil {
		return "", err
	}
	initBytes, err := readRegularFile(options.SandboxInitPath, maximumInitSize, "Sandbox Init")
	if err != nil {
		return "", err
	}
	if len(initBytes) == 0 {
		return "", errors.New("Sandbox Init must not be empty")
	}

	rootfsFile, err := os.CreateTemp(filepath.Dir(options.OutputPath), ".python-data-v1-rootfs-*.tar")
	if err != nil {
		return "", fmt.Errorf("create root filesystem component: %w", err)
	}
	rootfsPath := rootfsFile.Name()
	defer os.Remove(rootfsPath)
	if err := rootfsFile.Close(); err != nil {
		return "", fmt.Errorf("close root filesystem component: %w", err)
	}
	if err := buildRootFS(options.SourceCache, rootfsPath, lock); err != nil {
		return "", err
	}

	rootfsInfo, err := os.Stat(rootfsPath)
	if err != nil {
		return "", fmt.Errorf("stat root filesystem component: %w", err)
	}
	rootfsDigest, err := digestFile(rootfsPath)
	if err != nil {
		return "", err
	}

	parts := []bundlePart{
		newMemoryPart(bundleV1Components[0], lockBytes),
		newMemoryPart(bundleV1Components[1], policyBytes),
		newMemoryPart(bundleV1Components[2], initBytes),
		{
			component: component{
				Role:   bundleV1Components[3].Role,
				Path:   bundleV1Components[3].Path,
				Format: bundleV1Components[3].Format,
				Mode:   bundleV1Components[3].Mode,
				Size:   rootfsInfo.Size(),
				Digest: "sha256:" + rootfsDigest,
			},
			filePath: rootfsPath,
		},
	}
	manifestBytes, err := canonicalJSON(manifest{
		Schema:     manifestSchema,
		Profile:    profileID,
		Target:     lock.Target,
		Python:     lock.Python,
		Components: components(parts),
	})
	if err != nil {
		return "", fmt.Errorf("encode Profile Bundle manifest: %w", err)
	}

	digest, err := writeBundle(options.OutputPath, manifestBytes, parts)
	if err != nil {
		return "", err
	}
	return digest, nil
}

func readSystemCallPolicy(policyPath string) ([]byte, error) {
	policyBytes, err := readRegularFile(policyPath, maximumPolicySize, "System Call Policy")
	if err != nil {
		return nil, err
	}
	return decodeSystemCallPolicy(policyBytes)
}

func decodeSystemCallPolicy(policyBytes []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(policyBytes))
	decoder.DisallowUnknownFields()
	var policy systemCallPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("decode System Call Policy: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode System Call Policy: %w", err)
	}
	if policy.Schema != "system-call-policy/v1" || policy.Profile != profileID ||
		policy.Target.OS != "linux" || policy.Target.Arch != "amd64" {
		return nil, errors.New("System Call Policy must target system-call-policy/v1 for python-data-v1 on linux/amd64")
	}
	if policy.DefaultAction.Action != "errno" || policy.DefaultAction.Errno <= 0 {
		return nil, errors.New("System Call Policy must provide a nonzero default errno action")
	}
	if err := validateSystemCallRules(policy.Rules); err != nil {
		return nil, err
	}
	canonical, err := canonicalJSON(policy)
	if err != nil {
		return nil, fmt.Errorf("encode canonical System Call Policy: %w", err)
	}
	return canonical, nil
}

func validateSystemCallRules(rules []systemCallPolicyRule) error {
	if rules == nil {
		return errors.New("System Call Policy rules must be an explicit JSON array")
	}
	lastRuleName := ""
	for ruleIndex, rule := range rules {
		if len(rule.Names) == 0 || rule.Action != "allow" {
			return errors.New("System Call Policy rules must name syscalls and use the allow action")
		}
		for nameIndex, name := range rule.Names {
			if !validSystemCallName(name) {
				return errors.New("System Call Policy syscall names may contain only lowercase ASCII letters, digits, and underscores")
			}
			if nameIndex > 0 && rule.Names[nameIndex-1] >= name {
				return errors.New("System Call Policy syscall names must be unique and in bytewise order")
			}
		}
		if ruleIndex > 0 && lastRuleName >= rule.Names[0] {
			return errors.New("System Call Policy rules must be in bytewise syscall order")
		}
		lastRuleName = rule.Names[len(rule.Names)-1]
		if rule.Arguments == nil {
			return errors.New("System Call Policy rule arguments must be an explicit JSON array")
		}
		for argumentIndex, argument := range rule.Arguments {
			if argument.Index > 5 {
				return errors.New("System Call Policy argument index must be between 0 and 5")
			}
			if argumentIndex > 0 && rule.Arguments[argumentIndex-1].Index >= argument.Index {
				return errors.New("System Call Policy arguments must have unique increasing indexes")
			}
			switch argument.Operator {
			case "equal", "not-equal", "less-than", "less-or-equal", "greater-than", "greater-or-equal":
				if argument.Mask != 0 {
					return errors.New("System Call Policy argument mask is valid only with masked-equal")
				}
			case "masked-equal":
				if argument.Mask == 0 {
					return errors.New("System Call Policy masked-equal argument requires a nonzero mask")
				}
			default:
				return fmt.Errorf("unsupported System Call Policy argument operator %q", argument.Operator)
			}
		}
	}
	return nil
}

func validSystemCallName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validateBuildOptions(options BuildOptions) error {
	missing := make([]string, 0, 5)
	for name, value := range map[string]string{
		"--lock":               options.LockPath,
		"--source-cache":       options.SourceCache,
		"--system-call-policy": options.SystemCallPolicyPath,
		"--sandbox-init":       options.SandboxInitPath,
		"--output":             options.OutputPath,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("missing required options: %s", strings.Join(missing, ", "))
	}
	return nil
}

func readBuildLock(lockPath string) (buildLock, []byte, error) {
	lockBytes, err := readRegularFile(lockPath, maximumPolicySize, "profile build lock")
	if err != nil {
		return buildLock{}, nil, err
	}
	return decodeBuildLock(lockBytes)
}

func decodeBuildLock(lockBytes []byte) (buildLock, []byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(lockBytes))
	decoder.DisallowUnknownFields()
	var lock buildLock
	if err := decoder.Decode(&lock); err != nil {
		return buildLock{}, nil, fmt.Errorf("decode profile build lock: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return buildLock{}, nil, fmt.Errorf("decode profile build lock: %w", err)
	}
	if err := validateBuildLock(lock); err != nil {
		return buildLock{}, nil, err
	}
	canonical, err := canonicalJSON(lock)
	if err != nil {
		return buildLock{}, nil, fmt.Errorf("encode profile build lock: %w", err)
	}
	return lock, canonical, nil
}

func validateBuildLock(lock buildLock) error {
	if lock.Schema != buildLockSchema {
		return fmt.Errorf("unsupported profile build lock schema %q", lock.Schema)
	}
	if lock.Profile != profileID {
		return fmt.Errorf("profile build lock selects %q, want %q", lock.Profile, profileID)
	}
	if lock.Target.OS != "linux" || lock.Target.Arch != "amd64" {
		return fmt.Errorf("profile build lock target is %s/%s, want linux/amd64", lock.Target.OS, lock.Target.Arch)
	}
	if lock.Python.Version == "" || lock.Python.Entrypoint == "" {
		return errors.New("profile build lock must pin Python version and entrypoint")
	}
	if !path.IsAbs(lock.Python.Entrypoint) || validateRelativePath(strings.TrimPrefix(lock.Python.Entrypoint, "/")) != nil {
		return errors.New("profile build lock Python entrypoint must be a canonical absolute path")
	}
	for index, pythonPackage := range lock.Python.Packages {
		if pythonPackage.Name == "" || pythonPackage.Version == "" {
			return errors.New("profile build lock Python packages must pin name and version")
		}
		if index > 0 && lock.Python.Packages[index-1].Name >= pythonPackage.Name {
			return errors.New("profile build lock Python packages must have unique names in bytewise order")
		}
		if strings.Contains(strings.ToLower(pythonPackage.Version), "latest") || strings.ContainsAny(pythonPackage.Version, "*<>, ") {
			return fmt.Errorf("profile build lock Python package %q version must be exact", pythonPackage.Name)
		}
		if _, err := parseDigest(pythonPackage.Digest); err != nil {
			return fmt.Errorf("profile build lock Python package %q digest: %w", pythonPackage.Name, err)
		}
	}
	if len(lock.Inputs) == 0 {
		return errors.New("profile build lock must declare at least one external input")
	}
	for index, input := range lock.Inputs {
		if index > 0 && lock.Inputs[index-1].ID >= input.ID {
			return errors.New("profile build lock inputs must have unique IDs in bytewise order")
		}
		if err := validateLockedInput(input); err != nil {
			return fmt.Errorf("profile build lock input %q: %w", input.ID, err)
		}
	}
	return nil
}

func validateLockedInput(input lockedInput) error {
	if input.ID == "" || input.Version == "" || input.Filename == "" {
		return errors.New("id, version, and filename are required")
	}
	if input.Kind != "python-install-only" && input.Kind != "rootfs-overlay" {
		return fmt.Errorf("unsupported kind %q", input.Kind)
	}
	if strings.Contains(strings.ToLower(input.Version), "latest") || strings.ContainsAny(input.Version, "*<>, ") {
		return errors.New("version must be exact and must not be floating")
	}
	parsedSource, err := url.Parse(input.Source)
	if err != nil || parsedSource.Scheme != "https" || parsedSource.Host == "" {
		return errors.New("source must be an absolute HTTPS URL")
	}
	if filepath.Base(input.Filename) != input.Filename {
		return errors.New("filename must not contain a path")
	}
	if input.Size <= 0 || input.Size > maximumInputSize {
		return fmt.Errorf("size must be between 1 and %d bytes", maximumInputSize)
	}
	if _, err := parseDigest(input.Digest); err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	if input.ArchivePrefix != "" && !safeArchivePrefix(input.ArchivePrefix) {
		return errors.New("archive prefix must be empty or a safe relative directory path")
	}
	if input.Destination != "" && !safeArchivePrefix(input.Destination) {
		return errors.New("destination must be empty or a safe relative directory path")
	}
	if err := validatePathList("include", input.Include); err != nil {
		return err
	}
	if err := validatePathList("exclude", input.Exclude); err != nil {
		return err
	}
	return nil
}

func validatePathList(name string, values []string) error {
	for index, value := range values {
		if err := validateRelativePath(value); err != nil {
			return fmt.Errorf("%s path %q: %w", name, value, err)
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%s paths must be unique and in bytewise order", name)
		}
	}
	return nil
}

func buildRootFS(sourceCache, outputPath string, lock buildLock) error {
	spoolDirectory, err := os.MkdirTemp(filepath.Dir(outputPath), ".python-data-v1-rootfs-spool-*")
	if err != nil {
		return fmt.Errorf("create root filesystem spool: %w", err)
	}
	defer os.RemoveAll(spoolDirectory)

	entries := make([]rootfsBuildEntry, 0)
	seenEntrypoint := false
	budget := rootfsBuildBudget{}
	for _, input := range lock.Inputs {
		sourcePath := filepath.Join(sourceCache, input.Filename)
		providedEntrypoint, err := spoolLockedInput(sourcePath, spoolDirectory, input, lock.Python.Entrypoint, &entries, &budget)
		if err != nil {
			return err
		}
		seenEntrypoint = seenEntrypoint || providedEntrypoint
	}
	if !seenEntrypoint {
		return fmt.Errorf("locked inputs do not provide entrypoint %q", lock.Python.Entrypoint)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].name < entries[right].name
	})
	for index := 1; index < len(entries); index++ {
		if entries[index-1].name == entries[index].name {
			return fmt.Errorf("locked inputs map more than one entry to %q", entries[index].name)
		}
	}
	if err := validateRootFSItemClosure(entries); err != nil {
		return err
	}
	if err := validateRootFSEntrypoint(entries, lock.Python.Entrypoint); err != nil {
		return err
	}

	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("open root filesystem component: %w", err)
	}
	tarWriter := tar.NewWriter(output)
	for _, entry := range entries {
		canonical := canonicalTarHeader(entry.name, entry.typeFlag, entry.mode, entry.size, entry.linkName)
		if err := tarWriter.WriteHeader(canonical); err != nil {
			output.Close()
			return fmt.Errorf("write root filesystem entry %q: %w", entry.name, err)
		}
		if entry.typeFlag != tar.TypeReg {
			continue
		}
		spool, err := os.Open(entry.spool)
		if err != nil {
			output.Close()
			return fmt.Errorf("open spooled root filesystem file %q: %w", entry.name, err)
		}
		if _, err := io.CopyN(tarWriter, spool, entry.size); err != nil {
			spool.Close()
			output.Close()
			return fmt.Errorf("write root filesystem contents %q: %w", entry.name, err)
		}
		if err := spool.Close(); err != nil {
			output.Close()
			return fmt.Errorf("close spooled root filesystem file %q: %w", entry.name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		output.Close()
		return fmt.Errorf("close root filesystem tar: %w", err)
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return fmt.Errorf("sync root filesystem tar: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close root filesystem tar: %w", err)
	}
	return nil
}

func validateRootFSItemClosure(entries []rootfsBuildEntry) error {
	directories := make(map[string]struct{})
	entryPaths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entryPaths[entry.name] = struct{}{}
	}
	for _, entry := range entries {
		directory := path.Dir(entry.name)
		current := ""
		for _, segment := range strings.Split(directory, "/") {
			if segment == "." {
				continue
			}
			if current == "" {
				current = segment
			} else {
				current = path.Join(current, segment)
			}
			if _, occupied := entryPaths[current]; occupied {
				return fmt.Errorf("root filesystem entry %q cannot be the parent of %q", current, entry.name)
			}
			directories[current] = struct{}{}
			if len(entries)+len(directories) > maximumRootFSItems {
				return fmt.Errorf("root filesystem files, links, and implicit directories exceed the v1 limit of %d entries", maximumRootFSItems)
			}
		}
	}
	return nil
}

func validateRootFSEntrypoint(entries []rootfsBuildEntry, entrypoint string) error {
	byName := make(map[string]rootfsBuildEntry, len(entries))
	for _, entry := range entries {
		byName[entry.name] = entry
	}
	current := strings.TrimPrefix(entrypoint, "/")
	for range 40 {
		entry, ok := byName[current]
		if !ok {
			return fmt.Errorf("Python entrypoint %q is missing from the locked root filesystem", entrypoint)
		}
		switch entry.typeFlag {
		case tar.TypeReg:
			if entry.mode != 0o555 {
				return fmt.Errorf("Python entrypoint %q does not resolve to an executable regular file", entrypoint)
			}
			return nil
		case tar.TypeSymlink:
			current = path.Clean(path.Join(path.Dir(current), entry.linkName))
			if err := validateRelativePath(current); err != nil {
				return fmt.Errorf("Python entrypoint %q has an unsafe symlink chain: %w", entrypoint, err)
			}
		default:
			return fmt.Errorf("Python entrypoint %q has an unsupported file type", entrypoint)
		}
	}
	return fmt.Errorf("Python entrypoint %q has a symbolic-link cycle", entrypoint)
}

func spoolLockedInput(sourcePath, spoolDirectory string, input lockedInput, entrypoint string, entries *[]rootfsBuildEntry, budget *rootfsBuildBudget) (bool, error) {
	sourceBytes, err := readLockedSource(sourcePath, input)
	if err != nil {
		return false, err
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(sourceBytes))
	if err != nil {
		return false, fmt.Errorf("open locked source %q: %w", input.ID, err)
	}
	defer gzipReader.Close()

	sourceTar := tar.NewReader(gzipReader)
	seenEntrypoint := false
	for {
		header, err := sourceTar.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read locked source %q: %w", input.ID, err)
		}
		budget.scannedItems++
		if budget.scannedItems > maximumRootFSItems {
			return false, fmt.Errorf("locked sources exceed the v1 scan limit of %d entries", maximumRootFSItems)
		}
		if header.Size < 0 || budget.scannedBytes > maximumRootFSSize-header.Size {
			return false, fmt.Errorf("locked sources exceed the v1 scan budget of %d bytes", maximumRootFSSize)
		}
		budget.scannedBytes += header.Size
		mappedName, include, err := mapSourcePath(header.Name, input)
		if err != nil {
			return false, err
		}
		if !include {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
			if header.Size > maximumRootFSFile || budget.outputBytes > maximumRootFSSize-header.Size {
				return false, fmt.Errorf("root filesystem file %q exceeds the v1 size budget", mappedName)
			}
			budget.outputBytes += header.Size
			mode := int64(0o444)
			if header.Mode&0o111 != 0 {
				mode = 0o555
			}
			spool, err := os.CreateTemp(spoolDirectory, "entry-*")
			if err != nil {
				return false, fmt.Errorf("spool root filesystem file %q: %w", mappedName, err)
			}
			spoolPath := spool.Name()
			if _, err := io.CopyN(spool, sourceTar, header.Size); err != nil {
				spool.Close()
				return false, fmt.Errorf("spool root filesystem contents %q: %w", mappedName, err)
			}
			if err := spool.Close(); err != nil {
				return false, fmt.Errorf("close spooled root filesystem file %q: %w", mappedName, err)
			}
			*entries = append(*entries, rootfsBuildEntry{
				name:     mappedName,
				typeFlag: tar.TypeReg,
				mode:     mode,
				size:     header.Size,
				spool:    spoolPath,
			})
		case tar.TypeSymlink:
			if err := validateSymlink(mappedName, header.Linkname); err != nil {
				return false, err
			}
			*entries = append(*entries, rootfsBuildEntry{
				name:     mappedName,
				typeFlag: tar.TypeSymlink,
				mode:     0o555,
				linkName: header.Linkname,
			})
		default:
			return false, fmt.Errorf("locked source %q entry %q has unsupported type %d", input.ID, header.Name, header.Typeflag)
		}
		budget.outputItems++
		if budget.outputItems > maximumRootFSItems {
			return false, fmt.Errorf("root filesystem exceeds the v1 limit of %d entries", maximumRootFSItems)
		}
		if "/"+mappedName == entrypoint {
			seenEntrypoint = true
		}
	}
	return seenEntrypoint, nil
}

func readLockedSource(sourcePath string, input lockedInput) ([]byte, error) {
	file, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open locked source %q: %w", input.ID, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat locked source %q: %w", input.ID, err)
	}
	if !info.Mode().IsRegular() || info.Size() != input.Size {
		return nil, fmt.Errorf("locked source %q must be a regular file of exactly %d bytes", input.ID, input.Size)
	}
	contents, err := io.ReadAll(io.LimitReader(file, input.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read locked source %q: %w", input.ID, err)
	}
	if int64(len(contents)) != input.Size {
		return nil, fmt.Errorf("locked source %q changed while being read", input.ID)
	}
	wantDigest, _ := parseDigest(input.Digest)
	if actualDigest := digestBytes(contents); actualDigest != wantDigest {
		return nil, fmt.Errorf("locked source %q SHA-256 is %s, want %s", input.ID, actualDigest, wantDigest)
	}
	return contents, nil
}

func mapSourcePath(sourceName string, input lockedInput) (string, bool, error) {
	normalizedSource := strings.TrimPrefix(sourceName, "./")
	normalizedSource = strings.TrimSuffix(normalizedSource, "/")
	if normalizedSource == "" {
		return "", false, nil
	}
	if err := validateRelativePath(normalizedSource); err != nil {
		return "", false, fmt.Errorf("locked source %q entry %q: %w", input.ID, sourceName, err)
	}
	relative := normalizedSource
	if input.ArchivePrefix != "" {
		if !strings.HasPrefix(normalizedSource, input.ArchivePrefix) {
			return "", false, nil
		}
		relative = strings.TrimPrefix(normalizedSource, input.ArchivePrefix)
	}
	if relative == "" {
		return "", false, nil
	}
	if err := validateRelativePath(relative); err != nil {
		return "", false, fmt.Errorf("locked source %q entry %q: %w", input.ID, sourceName, err)
	}
	if len(input.Include) != 0 && !pathSelected(relative, input.Include) {
		return "", false, nil
	}
	for _, excluded := range input.Exclude {
		if relative == excluded || strings.HasPrefix(relative, strings.TrimSuffix(excluded, "/")+"/") {
			return "", false, nil
		}
	}
	mapped := relative
	if input.Destination != "" {
		mapped = path.Join(strings.TrimSuffix(input.Destination, "/"), relative)
	}
	if err := validateRelativePath(mapped); err != nil {
		return "", false, fmt.Errorf("mapped root filesystem entry %q: %w", mapped, err)
	}
	return mapped, true, nil
}

func pathSelected(relative string, included []string) bool {
	for _, candidate := range included {
		if relative == candidate || strings.HasPrefix(relative, candidate+"/") {
			return true
		}
	}
	return false
}

func validateSymlink(name, target string) error {
	if target == "" || path.IsAbs(target) || strings.Contains(target, "\\") {
		return fmt.Errorf("root filesystem symlink %q has unsafe target %q", name, target)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("root filesystem symlink %q escapes through target %q", name, target)
	}
	return nil
}

func canonicalTarHeader(name string, typeFlag byte, mode, size int64, linkName string) *tar.Header {
	return &tar.Header{
		Name:       name,
		Linkname:   linkName,
		Size:       size,
		Mode:       mode,
		Uid:        0,
		Gid:        0,
		Uname:      "",
		Gname:      "",
		ModTime:    canonicalTime,
		AccessTime: time.Time{},
		ChangeTime: time.Time{},
		Typeflag:   typeFlag,
		Format:     tar.FormatPAX,
	}
}

func newMemoryPart(contract component, contents []byte) bundlePart {
	return bundlePart{
		component: component{
			Role:   contract.Role,
			Path:   contract.Path,
			Format: contract.Format,
			Mode:   contract.Mode,
			Size:   int64(len(contents)),
			Digest: "sha256:" + digestBytes(contents),
		},
		contents: contents,
	}
}

func components(parts []bundlePart) []component {
	result := make([]component, len(parts))
	for index := range parts {
		result[index] = parts[index].component
	}
	return result
}

func writeBundle(outputPath string, manifestBytes []byte, parts []bundlePart) (string, error) {
	if _, err := os.Lstat(outputPath); err == nil {
		return "", fmt.Errorf("output %q already exists", outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect output %q: %w", outputPath, err)
	}
	outputDirectory := filepath.Dir(outputPath)
	temporary, err := os.CreateTemp(outputDirectory, ".profile-bundle-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary Profile Bundle: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	temporaryClosed := false
	defer func() {
		if !temporaryClosed {
			temporary.Close()
		}
	}()

	digest := sha256.New()
	tarWriter := tar.NewWriter(io.MultiWriter(temporary, digest))
	if err := writeOuterEntry(tarWriter, "manifest.json", 0o444, manifestBytes, ""); err != nil {
		return "", err
	}
	for _, part := range parts {
		if part.filePath == "" {
			if err := writeOuterEntry(tarWriter, part.component.Path, parseMode(part.component.Mode), part.contents, ""); err != nil {
				return "", err
			}
			continue
		}
		file, err := os.Open(part.filePath)
		if err != nil {
			return "", fmt.Errorf("open component %q: %w", part.component.Role, err)
		}
		header := canonicalTarHeader(part.component.Path, tar.TypeReg, parseMode(part.component.Mode), part.component.Size, "")
		header.Format = tar.FormatUSTAR
		if err := tarWriter.WriteHeader(header); err != nil {
			file.Close()
			return "", fmt.Errorf("write component %q header: %w", part.component.Role, err)
		}
		if _, err := io.CopyN(tarWriter, file, part.component.Size); err != nil {
			file.Close()
			return "", fmt.Errorf("write component %q: %w", part.component.Role, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close component %q: %w", part.component.Role, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return "", fmt.Errorf("close Profile Bundle tar: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync Profile Bundle: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close Profile Bundle: %w", err)
	}
	temporaryClosed = true
	if err := os.Link(temporaryPath, outputPath); err != nil {
		return "", fmt.Errorf("publish Profile Bundle: %w", err)
	}
	if err := syncDirectory(outputDirectory); err != nil {
		if removeErr := os.Remove(outputPath); removeErr != nil {
			return "", fmt.Errorf("publish Profile Bundle: %v; remove unsynced output: %w", err, removeErr)
		}
		return "", fmt.Errorf("publish Profile Bundle: %w", err)
	}
	_ = os.Remove(temporaryPath)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeOuterEntry(writer *tar.Writer, name string, mode int64, contents []byte, linkName string) error {
	header := canonicalTarHeader(name, tar.TypeReg, mode, int64(len(contents)), linkName)
	header.Format = tar.FormatUSTAR
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write Profile Bundle entry %q header: %w", name, err)
	}
	if _, err := writer.Write(contents); err != nil {
		return fmt.Errorf("write Profile Bundle entry %q: %w", name, err)
	}
	return nil
}

func parseMode(mode string) int64 {
	if mode == "0555" {
		return 0o555
	}
	return 0o444
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func parseDigest(value string) (string, error) {
	if !strings.HasPrefix(value, "sha256:") {
		return "", errors.New("must use sha256:<lowercase-hex>")
	}
	hexDigest := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(hexDigest)
	if err != nil || len(decoded) != sha256.Size || hexDigest != strings.ToLower(hexDigest) {
		return "", errors.New("must use sha256:<64-lowercase-hex>")
	}
	return hexDigest, nil
}

func digestBytes(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func digestFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open %q for SHA-256: %w", filePath, err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("calculate SHA-256 for %q: %w", filePath, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func readRegularFile(filePath string, maximumSize int64, description string) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", description, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened %s: %w", description, err)
	}
	pathInfo, err := os.Lstat(filePath)
	if err != nil {
		return nil, fmt.Errorf("lstat %s after opening: %w", description, err)
	}
	if !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		return nil, fmt.Errorf("%s must be a regular file", description)
	}
	if openedInfo.Size() < 0 || openedInfo.Size() > maximumSize {
		return nil, fmt.Errorf("%s exceeds %d bytes", description, maximumSize)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", description, err)
	}
	if int64(len(contents)) != openedInfo.Size() {
		return nil, fmt.Errorf("%s changed while being read", description)
	}
	return contents, nil
}

func safeArchivePrefix(value string) bool {
	return strings.HasSuffix(value, "/") && validateRelativePath(strings.TrimSuffix(value, "/")) == nil
}

func validateRelativePath(value string) error {
	if value == "" || len(value) > maximumPathLength || path.IsAbs(value) || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) {
		return errors.New("path must be a non-empty slash-relative path")
	}
	if path.Clean(value) != value {
		return errors.New("path must be canonical and contain no dot segments")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || len(segment) > 255 || segment == "." || segment == ".." {
			return errors.New("path contains an unsafe segment")
		}
	}
	return nil
}
