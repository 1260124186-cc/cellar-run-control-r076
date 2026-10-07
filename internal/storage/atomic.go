package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// writeSnapshot encodes a value as indented JSON and commits it atomically.
func writeSnapshot(path string, value any) error {
	buffer, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	buffer = append(buffer, '\n')
	return writeBytesAtomic(path, buffer)
}

// writeBytesAtomic commits raw bytes to durable storage and renames the
// result over the target, so readers never observe a partially written
// file. The directory is synced afterwards so the rename itself survives
// a power loss.
func writeBytesAtomic(path string, content []byte) error {
	directory := filepath.Dir(path)
	handle, err := os.CreateTemp(directory, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tempName := handle.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()

	if _, err := handle.Write(content); err != nil {
		_ = handle.Close()
		return fmt.Errorf("write state bytes: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("commit state bytes: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	if err := syncDir(directory); err != nil {
		return fmt.Errorf("commit state directory: %w", err)
	}
	removeTemp = false
	return nil
}

func syncDir(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}
