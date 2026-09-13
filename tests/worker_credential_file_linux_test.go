//go:build linux

package workspace_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

func TestWorkerCredentialFileRequiresPrivateOwnedRegularFile(t *testing.T) {
	const token = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	directory := t.TempDir()
	private := filepath.Join(directory, "private")
	if err := os.WriteFile(private, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := workercredential.LoadFile(private); err != nil || got != token {
		t.Fatalf("private credential file: %v", err)
	}
	for _, mode := range []os.FileMode{0644, 0640, 0604} {
		if err := os.Chmod(private, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := workercredential.LoadFile(private); err == nil {
			t.Fatalf("accepted exposed mode %o", mode)
		}
	}
	if err := os.Chmod(private, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, directory} {
		if _, err := workercredential.LoadFile(path); err == nil {
			t.Fatal("accepted symbolic link or special file")
		}
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(private, 65534, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := workercredential.LoadFile(private); err == nil {
			t.Fatal("accepted another account's file")
		}
	}
}

func TestWorkerCredentialFileRejectsMalformedAndOversizedValues(t *testing.T) {
	for _, content := range []string{"", "not-a-credential", strings.Repeat("A", 4096), strings.Repeat("A", 43) + "\n" + strings.Repeat("A", 43)} {
		path := filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := workercredential.LoadFile(path); err == nil {
			t.Fatal("accepted malformed credential")
		}
	}
}
