//go:build linux

package sandboxsupervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// These are trusted admission limits, independent of the writable Workspace.
// Requests can select staged inputs, but cannot change the limits or root.
const (
	maximumAttachments     = 16
	maximumAttachmentBytes = int64(20 << 20)
	maximumAttachmentTotal = int64(100 << 20)
	attachmentPathOpenFlag = 0x200000 // Linux O_PATH; open without accessing a device/FIFO.
	firstAttachmentFD      = 7
)

type openedAttachment struct {
	name string
	file *os.File
}

type bootstrapPolicy struct {
	Storage         StorageBudget `json:"storage"`
	AttachmentNames []string      `json:"attachment_names,omitempty"`
}

// encoding/json otherwise matches nested struct fields case-insensitively.
// Keep the public nested object as closed as the top-level protocol object.
func (input *AttachmentInput) UnmarshalJSON(payload []byte) error {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return errors.New("Attachment reference must be an object")
	}
	type plainAttachmentInput AttachmentInput
	var decoded plainAttachmentInput
	if err := decodeStrictJSON(payload, &decoded, "staging_id", "name"); err != nil {
		return err
	}
	*input = AttachmentInput(decoded)
	return nil
}

func validAttachmentName(name string) bool {
	if len(name) == 0 || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// Walk the trusted absolute path without following any symlink, including in
// ancestors. A writable ancestor can cause denial of service by replacement,
// but cannot redirect the retained handle or pass the later inode comparison.
func openAbsoluteAttachmentPath(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("Attachment path must be absolute and clean")
	}
	fd, err := syscall.Open("/", attachmentPathOpenFlag|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if path != "/" {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		for index, part := range parts {
			flags := attachmentPathOpenFlag | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
			if index < len(parts)-1 {
				flags |= syscall.O_DIRECTORY
			}
			next, openErr := syscall.Openat(fd, part, flags, 0)
			_ = syscall.Close(fd)
			if openErr != nil {
				return nil, openErr
			}
			fd = next
		}
	}
	return os.NewFile(uintptr(fd), "attachment-path"), nil
}

func openAttachmentRoot(path string) (*os.Root, error) {
	directory, err := openAbsoluteAttachmentPath(path)
	if err != nil {
		return nil, fmt.Errorf("open Attachment root: %w", err)
	}
	defer directory.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(directory.Fd()), &stat); err != nil {
		return nil, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || int(stat.Uid) != os.Geteuid() || stat.Mode&0o022 != 0 {
		return nil, errors.New("Attachment root must be a Supervisor-owned directory without group or other writes")
	}
	// The descriptor path is constructed internally. Opening the os.Root from
	// it retains the exact directory just inspected, even if a parent moves.
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", directory.Fd()))
	if err != nil {
		return nil, fmt.Errorf("retain Attachment root: %w", err)
	}
	return root, nil
}

func resolveAttachments(root *os.Root, inputs []AttachmentInput) ([]openedAttachment, ErrorCode) {
	if len(inputs) > maximumAttachments {
		return nil, ErrorCodeInvalidReference
	}
	names := make(map[string]bool, len(inputs))
	identifiers := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		if !validOpaqueIdentifier(input.StagingID) || !validAttachmentName(input.Name) || names[input.Name] || identifiers[input.StagingID] {
			return nil, ErrorCodeInvalidReference
		}
		names[input.Name], identifiers[input.StagingID] = true, true
	}
	if len(inputs) == 0 {
		return nil, ""
	}
	if root == nil {
		return nil, ErrorCodeOperationUnavailable
	}
	opened := make([]openedAttachment, 0, len(inputs))
	complete := false
	defer func() {
		if !complete {
			closeAttachments(opened)
		}
	}()
	var total int64
	for _, input := range inputs {
		file, err := root.OpenFile(input.StagingID, attachmentPathOpenFlag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, ErrorCodeInvalidReference
		}
		opened = append(opened, openedAttachment{name: input.Name, file: file})
		var stat syscall.Stat_t
		if err := syscall.Fstat(int(file.Fd()), &stat); err != nil || !validStagedAttachment(stat) {
			return nil, ErrorCodeInvalidReference
		}
		if stat.Size > maximumAttachmentTotal-total {
			return nil, ErrorCodeInvalidReference
		}
		total += stat.Size
	}
	complete = true
	return opened, ""
}

func validStagedAttachment(stat syscall.Stat_t) bool {
	return stat.Mode == syscall.S_IFREG|0o444 && stat.Uid == 0 && stat.Nlink == 1 && stat.Size >= 0 && stat.Size <= maximumAttachmentBytes
}

func closeAttachments(attachments []openedAttachment) {
	for _, attachment := range attachments {
		_ = attachment.file.Close()
	}
}

// Parent-namespace file descriptors cannot be bind-mount sources after
// CLONE_NEWNS. Reopen the selected inode in this namespace before /tmp or the
// old root is hidden; never trust the path without checking its identity.
func reopenBootstrapAttachments(names []string) ([]openedAttachment, error) {
	if len(names) > maximumAttachments {
		return nil, errors.New("too many bootstrap Attachments")
	}
	opened := make([]openedAttachment, 0, len(names))
	complete := false
	defer func() {
		if !complete {
			closeAttachments(opened)
		}
	}()
	seen := make(map[string]bool, len(names))
	var total int64
	for index, name := range names {
		inherited := os.NewFile(uintptr(firstAttachmentFD+index), "inherited-attachment")
		defer inherited.Close()
		if !validAttachmentName(name) || seen[name] {
			return nil, errors.New("invalid bootstrap Attachment name")
		}
		seen[name] = true
		var before syscall.Stat_t
		if err := syscall.Fstat(int(inherited.Fd()), &before); err != nil {
			return nil, err
		}
		// Host UID 0 appears as the overflow UID in this User Namespace; trust
		// the parent's ownership validation and require exact metadata equality.
		if before.Mode != syscall.S_IFREG|0o444 || before.Nlink != 1 || before.Size < 0 || before.Size > maximumAttachmentBytes || before.Size > maximumAttachmentTotal-total {
			return nil, errors.New("invalid inherited Attachment metadata")
		}
		total += before.Size
		source, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", inherited.Fd()))
		if err != nil || strings.HasSuffix(source, " (deleted)") {
			return nil, errors.New("inherited Attachment has no live source path")
		}
		file, err := openAbsoluteAttachmentPath(source)
		if err != nil {
			return nil, fmt.Errorf("reopen Attachment in Sandbox mount namespace: %w", err)
		}
		opened = append(opened, openedAttachment{name: name, file: file})
		var after syscall.Stat_t
		if err := syscall.Fstat(int(file.Fd()), &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Uid != after.Uid || before.Gid != after.Gid || before.Nlink != after.Nlink || before.Size != after.Size {
			return nil, errors.New("Attachment source changed during bootstrap")
		}
		if err := inherited.Close(); err != nil {
			return nil, err
		}
	}
	complete = true
	return opened, nil
}

func mountSandboxAttachments(inputRoot string, attachments []openedAttachment) error {
	const restrictions = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	if err := syscall.Mount("tmpfs", inputRoot, "tmpfs", restrictions, "size=1m,nr_inodes=17,mode=0555"); err != nil {
		return fmt.Errorf("mount immutable input directory: %w", err)
	}
	for _, attachment := range attachments {
		target := inputRoot + "/" + attachment.name
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := syscall.Mount(fmt.Sprintf("/proc/self/fd/%d", attachment.file.Fd()), target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind selected Attachment: %w", err)
		}
		// Add every safety restriction. Omit all atime flags so Linux preserves
		// the source's existing atime policy (including locked parent flags).
		flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | restrictions)
		if err := syscall.Mount("", target, "", flags, ""); err != nil {
			return fmt.Errorf("make Attachment mount read-only: %w", err)
		}
		if err := attachment.file.Close(); err != nil {
			return err
		}
	}
	if err := syscall.Mount("", inputRoot, "", syscall.MS_REMOUNT|syscall.MS_RDONLY|restrictions, ""); err != nil {
		return fmt.Errorf("make input directory read-only: %w", err)
	}
	return nil
}
