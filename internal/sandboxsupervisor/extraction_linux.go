//go:build linux

package sandboxsupervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const maximumExtractedFiles = 16

func validOutputPaths(paths []string) bool {
	if len(paths) > maximumExtractedFiles {
		return false
	}
	seen := make(map[string]bool, len(paths))
	for _, name := range paths {
		if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") ||
			path.IsAbs(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") || path.Clean(name) != name || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

// O_PATH pins an object without opening a device or waiting on a FIFO. Linux's
// syscall package predates this flag; the Sandbox profile supports amd64.
const outputPathHandle = 0x200000

func extractDeclaredOutputs(paths []string, fileBytes, totalBytes int64) ([]ExtractedOutput, OutputExtractionError) {
	if len(paths) == 0 {
		return nil, ""
	}
	rootFD, err := syscall.Open("/workspace/output", outputPathHandle|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, OutputUnsafe
	}
	root := os.NewFile(uintptr(rootFD), "workspace-output")
	defer root.Close()
	var rootStat syscall.Stat_t
	if err := syscall.Fstat(rootFD, &rootStat); err != nil {
		return nil, OutputUnsafe
	}
	outputs := make([]ExtractedOutput, 0, len(paths))
	for _, name := range paths {
		output, code := extractOutput(root, rootStat.Dev, name, min(fileBytes, totalBytes))
		if code != "" {
			return nil, code // No partial batch can escape into an Execution Result.
		}
		totalBytes -= output.Size
		outputs = append(outputs, output)
	}
	return outputs, ""
}

func extractOutput(root *os.File, device uint64, name string, allowance int64) (ExtractedOutput, OutputExtractionError) {
	pinned, before, err := openOutputPath(root, device, name)
	if err != nil {
		return ExtractedOutput{}, OutputUnsafe
	}
	defer pinned.Close()
	initial := before[len(before)-1]
	if initial.Size < 0 || initial.Size > allowance {
		return ExtractedOutput{}, OutputLimit
	}
	// This proc path is assembled only from Init's own pinned, verified regular
	// descriptor. Reopening the caller's path here would introduce a TOCTOU gap.
	file, err := os.Open("/proc/self/fd/" + strconv.Itoa(int(pinned.Fd())))
	if err != nil {
		return ExtractedOutput{}, OutputUnsafe
	}
	defer file.Close()
	content := make([]byte, int(initial.Size))
	if _, err := io.ReadFull(file, content); err != nil {
		return ExtractedOutput{}, OutputUnsafe
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || err != io.EOF {
		return ExtractedOutput{}, OutputUnsafe
	}
	var after syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &after); err != nil || !sameOutputFile(initial, after) {
		return ExtractedOutput{}, OutputUnsafe
	}
	resolved, current, err := openOutputPath(root, device, name)
	if err != nil {
		return ExtractedOutput{}, OutputUnsafe
	}
	defer resolved.Close()
	// Check every directory's identity as well as the leaf. An opened parent
	// can survive a rename outside the root; it must still occupy its path.
	for index := range before {
		if before[index].Dev != current[index].Dev || before[index].Ino != current[index].Ino {
			return ExtractedOutput{}, OutputUnsafe
		}
	}
	if !sameOutputFile(initial, current[len(current)-1]) {
		return ExtractedOutput{}, OutputUnsafe
	}
	digest := sha256.Sum256(content)
	return ExtractedOutput{Path: name, Size: initial.Size, SHA256: hex.EncodeToString(digest[:]), Content: content}, ""
}

// Every component is opened relative to a directory handle with NOFOLLOW.
// The leaf is checked before any content access; links and special files never
// reach a read-capable open. The output tmpfs device is the only allowed one.
func openOutputPath(root *os.File, device uint64, name string) (*os.File, []syscall.Stat_t, error) {
	parent := root
	var owned *os.File
	defer func() {
		if owned != nil {
			_ = owned.Close()
		}
	}()
	components := strings.Split(name, "/")
	identities := make([]syscall.Stat_t, 0, len(components))
	for index, component := range components {
		flags := outputPathHandle | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
		last := index == len(components)-1
		if !last {
			flags |= syscall.O_DIRECTORY
		}
		fd, err := syscall.Openat(int(parent.Fd()), component, flags, 0)
		if err != nil {
			return nil, nil, err
		}
		file := os.NewFile(uintptr(fd), "declared-output")
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != nil || stat.Dev != device ||
			(last && (stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1)) {
			_ = file.Close()
			return nil, nil, errors.New("declared output must be a single-link regular file on the Workspace filesystem")
		}
		identities = append(identities, stat)
		if last {
			return file, identities, nil
		}
		if owned != nil {
			_ = owned.Close()
		}
		owned, parent = file, file
	}
	return nil, nil, errors.New("empty declared output path")
}

func sameOutputFile(before, after syscall.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Mode == after.Mode && before.Nlink == after.Nlink &&
		before.Uid == after.Uid && before.Gid == after.Gid && before.Size == after.Size && before.Mtim == after.Mtim && before.Ctim == after.Ctim
}

func extractedOutputBytes(outputs []ExtractedOutput) int64 {
	var total int64
	for _, output := range outputs {
		total += int64(len(output.Content))
	}
	return total
}

func validExtractedOutputs(result initResponse, paths []string, fileBytes, totalBytes int64) bool {
	if result.OutputError != "" {
		return len(paths) != 0 && len(result.Outputs) == 0 && (result.OutputError == OutputUnsafe || result.OutputError == OutputLimit)
	}
	if len(result.Outputs) != len(paths) {
		return false
	}
	for index, output := range result.Outputs {
		if output.Path != paths[index] || output.Size != int64(len(output.Content)) || output.Size > min(fileBytes, totalBytes) {
			return false
		}
		digest := sha256.Sum256(output.Content)
		if output.SHA256 != hex.EncodeToString(digest[:]) {
			return false
		}
		totalBytes -= output.Size
	}
	return true
}
