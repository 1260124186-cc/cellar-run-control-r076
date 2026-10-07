package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic commits contents to a temporary file in the same
// directory, syncs it to durable storage, and renames it over the target.
// Readers therefore see either the old file or the complete new file.
func writeFileAtomic(path string, contents []byte) error {
	directory := filepath.Dir(path)
	handle, err := os.CreateTemp(directory, ".migrate-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempName := handle.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()

	if _, err := handle.Write(contents); err != nil {
		_ = handle.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("commit file bytes: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	removeTemp = false
	return nil
}

// copyFileDurable duplicates a file so the original bytes survive later
// replacement of the target.
func copyFileDurable(source, target string) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(source), err)
	}
	return writeFileAtomic(target, contents)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", filepath.Base(path), err)
	}
	return nil
}
