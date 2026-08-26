//go:build !windows

package profilebundle

import (
	"fmt"
	"os"
)

func syncDirectory(directoryPath string) error {
	directory, err := os.Open(directoryPath)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return fmt.Errorf("sync directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close synced directory: %w", err)
	}
	return nil
}
