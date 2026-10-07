package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// StepReport is the inspectable outcome of one migration step.
type StepReport struct {
	Name        string
	FromVersion int
	ToVersion   int
	Status      StepStatus
	Checksum    string
	Detail      string
}

// Report summarizes one EnsureCurrent run for operators and logs.
type Report struct {
	InitialVersion int
	FinalVersion   int
	Resumed        bool
	BackupFile     string
	Steps          []StepReport
}

// EnsureCurrent brings the data directory to the current schema version.
// It detects the stored version, runs the approved migration chain in
// order, and records every step result in a durable journal. A previously
// interrupted migration is resumed from its last verified step. The live
// state file is replaced exactly once, atomically, after every step has
// been verified; until then the original data remains readable.
func EnsureCurrent(dataDir string) (*Report, error) {
	interrupted, err := journalExists(dataDir)
	if err != nil {
		return nil, err
	}
	if interrupted {
		return resumeMigration(dataDir)
	}
	return beginMigration(dataDir)
}

// beginMigration handles a data directory with no interrupted migration:
// detect the stored version and, when it is behind, start a new journal.
func beginMigration(dataDir string) (*Report, error) {
	report := &Report{FinalVersion: CurrentVersion}
	raw, err := os.ReadFile(statePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		// Nothing stored yet; the first write lands at the current version.
		return report, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	snapshot, version, err := DecodeState(raw)
	if err != nil {
		return nil, err
	}
	report.InitialVersion = version
	if version == CurrentVersion {
		return report, nil
	}
	steps, err := plan(version)
	if err != nil {
		return nil, err
	}

	backupName := fmt.Sprintf("state-v%d-%s.json",
		version, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Join(dataDir, backupDirName), 0o755); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	backupPath := filepath.Join(dataDir, backupDirName, backupName)
	if err := copyFileDurable(statePath(dataDir), backupPath); err != nil {
		return nil, fmt.Errorf("back up pre-migration state: %w", err)
	}
	report.BackupFile = filepath.Join(backupDirName, backupName)

	journal, err := newJournal(version, CurrentVersion, steps, report.BackupFile)
	if err != nil {
		return nil, err
	}
	if err := writeJournal(dataDir, journal); err != nil {
		return nil, err
	}
	if err := syncDir(dataDir); err != nil {
		return nil, err
	}
	return runSteps(dataDir, journal, snapshot, 0, report)
}

// resumeMigration continues a migration whose journal survived an
// interruption. Verified steps are trusted only when the staged data still
// matches the checksum recorded in the journal.
func resumeMigration(dataDir string) (*Report, error) {
	journal, err := readJournal(dataDir)
	if err != nil {
		return nil, err
	}
	report := &Report{
		InitialVersion: journal.FromVersion,
		FinalVersion:   CurrentVersion,
		Resumed:        true,
		BackupFile:     journal.BackupFile,
	}

	raw, err := os.ReadFile(statePath(dataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err == nil {
		_, version, detectErr := DecodeState(raw)
		if detectErr != nil {
			return nil, detectErr
		}
		switch version {
		case journal.ToVersion:
			// The cutover completed before the interruption; only the
			// journal cleanup remains.
			return finalize(dataDir, journal, report)
		case journal.FromVersion:
			// Migration never published; resume from the journal.
		default:
			return nil, fmt.Errorf(
				"state file is at version %d but the interrupted migration covers %d -> %d; inspect the data directory manually",
				version, journal.FromVersion, journal.ToVersion)
		}
	}

	start := journal.firstPending()
	var staged domain.Snapshot
	if start == 0 {
		if raw == nil {
			return nil, fmt.Errorf(
				"state file is missing and no migration step completed; run `fermentctl migrate rollback` to restore %s",
				journal.BackupFile)
		}
		original, _, decodeErr := DecodeState(raw)
		if decodeErr != nil {
			return nil, decodeErr
		}
		staged = original
	} else {
		loaded, version, loadErr := ReadState(stagingPath(dataDir))
		if loadErr != nil {
			return nil, fmt.Errorf("resume migration: %w", loadErr)
		}
		last := journal.Steps[start-1]
		switch {
		case version == last.ToVersion:
			// Normal resume point: the staged data must be byte-identical
			// to the result the journal recorded for the verified step.
			if err := matchJournalChecksum(stagingPath(dataDir), last); err != nil {
				return nil, err
			}
		case start < len(journal.Steps) && version == journal.Steps[start].ToVersion:
			// The interruption landed between the staging write and the
			// journal write. The staged data was never recorded, so accept
			// it only after the step's own verification passes again.
			migrations, planErr := plan(journal.FromVersion)
			if planErr != nil {
				return nil, planErr
			}
			if err := migrations[start].Verify(loaded); err != nil {
				return nil, fmt.Errorf("staged migration data fails verification: %w", err)
			}
			checksum, err := stagedChecksum(stagingPath(dataDir))
			if err != nil {
				return nil, err
			}
			now := time.Now().UTC()
			journal.Steps[start].Status = StepVerified
			journal.Steps[start].Checksum = checksum
			journal.Steps[start].AppliedAt = &now
			journal.Steps[start].Error = ""
			if err := writeJournal(dataDir, journal); err != nil {
				return nil, err
			}
			if err := syncDir(dataDir); err != nil {
				return nil, err
			}
			start++
		default:
			if start < len(journal.Steps) {
				return nil, fmt.Errorf(
					"staged migration data is version %d, expected version %d or %d from the journal",
					version, last.ToVersion, journal.Steps[start].ToVersion)
			}
			return nil, fmt.Errorf(
				"staged migration data is version %d, expected version %d from the journal",
				version, last.ToVersion)
		}
		staged = loaded
	}
	return runSteps(dataDir, journal, staged, start, report)
}

// matchJournalChecksum confirms the staged envelope is byte-identical to
// the result the journal recorded for a verified step.
func matchJournalChecksum(path string, record StepRecord) error {
	checksum, err := stagedChecksum(path)
	if err != nil {
		return err
	}
	if checksum != record.Checksum {
		return fmt.Errorf(
			"%w: staged migration data does not match journal step %q",
			ErrChecksumMismatch, record.Name)
	}
	return nil
}

// stagedChecksum reads the payload checksum recorded in a staged envelope.
func stagedChecksum(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read staged migration data: %w", err)
	}
	var framed envelope
	if err := json.Unmarshal(raw, &framed); err != nil {
		return "", fmt.Errorf("decode staged migration data: %w", err)
	}
	actual, err := payloadChecksum(framed.Snapshot)
	if err != nil {
		return "", err
	}
	if framed.Checksum == "" || framed.Checksum != actual {
		return "", fmt.Errorf("%w: staged migration data", ErrChecksumMismatch)
	}
	return framed.Checksum, nil
}

// runSteps applies and verifies each remaining step, recording every result
// in the journal before advancing, then publishes the final staged snapshot
// with one atomic rename.
func runSteps(
	dataDir string,
	journal Journal,
	staged domain.Snapshot,
	start int,
	report *Report,
) (*Report, error) {
	migrations, err := plan(journal.FromVersion)
	if err != nil {
		return nil, err
	}
	for index := start; index < len(migrations); index++ {
		step := migrations[index]
		next, err := step.Apply(staged)
		if err == nil {
			err = step.Verify(next)
		}
		var encoded []byte
		var checksum string
		if err == nil {
			encoded, checksum, err = encodeEnvelope(next, step.ToVersion)
		}
		if err == nil {
			err = writeFileAtomic(stagingPath(dataDir), encoded)
		}
		if err != nil {
			return failStep(dataDir, journal, index, report, err)
		}
		now := time.Now().UTC()
		journal.Steps[index].Status = StepVerified
		journal.Steps[index].Checksum = checksum
		journal.Steps[index].AppliedAt = &now
		journal.Steps[index].Error = ""
		if err := writeJournal(dataDir, journal); err != nil {
			return nil, err
		}
		if err := syncDir(dataDir); err != nil {
			return nil, err
		}
		staged = next
	}

	// Cutover: the single write the serving path can observe. Before this
	// rename the state file still holds the pre-migration version.
	if err := os.Rename(stagingPath(dataDir), statePath(dataDir)); err != nil {
		return nil, fmt.Errorf("publish migrated state: %w", err)
	}
	if err := syncDir(dataDir); err != nil {
		return nil, err
	}
	return finalize(dataDir, journal, report)
}

// failStep records a step failure in the journal so operators can inspect
// it, then reports the failure. The original state file is untouched.
func failStep(
	dataDir string,
	journal Journal,
	index int,
	report *Report,
	cause error,
) (*Report, error) {
	journal.Steps[index].Status = StepFailed
	journal.Steps[index].Error = cause.Error()
	_ = writeJournal(dataDir, journal)
	collectSteps(&journal, report)
	return report, fmt.Errorf(
		"migration step %q failed: %w (original data preserved; restart to resume or run `fermentctl migrate rollback`)",
		journal.Steps[index].Name, cause)
}

// finalize removes migration artifacts after a successful cutover and
// completes the report from the journal.
func finalize(dataDir string, journal Journal, report *Report) (*Report, error) {
	if err := removeIfExists(stagingPath(dataDir)); err != nil {
		return nil, err
	}
	if err := removeIfExists(journalPath(dataDir)); err != nil {
		return nil, err
	}
	if err := syncDir(dataDir); err != nil {
		return nil, err
	}
	report.FinalVersion = journal.ToVersion
	collectSteps(&journal, report)
	return report, nil
}

func collectSteps(journal *Journal, report *Report) {
	for _, record := range journal.Steps {
		report.Steps = append(report.Steps, StepReport{
			Name:        record.Name,
			FromVersion: record.FromVersion,
			ToVersion:   record.ToVersion,
			Status:      record.Status,
			Checksum:    record.Checksum,
			Detail:      record.Error,
		})
	}
}

// Status describes the data directory for the `migrate status` command.
type Status struct {
	DataDir        string
	StateExists    bool
	DataVersion    int
	BinaryVersion  int
	Pending        []Migration
	Journal        *Journal
	DetectionError string
}

// Inspect reports the stored schema version, the migrations that would
// run, and any interrupted migration, without modifying anything.
func Inspect(dataDir string) Status {
	status := Status{DataDir: dataDir, BinaryVersion: CurrentVersion}
	interrupted, err := journalExists(dataDir)
	if err != nil {
		status.DetectionError = err.Error()
		return status
	}
	if interrupted {
		journal, err := readJournal(dataDir)
		if err != nil {
			status.DetectionError = err.Error()
		} else {
			status.Journal = &journal
		}
	}
	raw, err := os.ReadFile(statePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return status
	}
	if err != nil {
		status.DetectionError = err.Error()
		return status
	}
	status.StateExists = true
	_, version, err := DecodeState(raw)
	if err != nil {
		status.DetectionError = err.Error()
		return status
	}
	status.DataVersion = version
	if version < CurrentVersion {
		steps, err := plan(version)
		if err != nil {
			status.DetectionError = err.Error()
			return status
		}
		status.Pending = steps
	}
	return status
}

// RollbackReport summarizes a Rollback run.
type RollbackReport struct {
	Action      string // "none", "discarded", or "restored"
	DataVersion int
	MigrationID string
	BackupFile  string
}

// Rollback abandons an interrupted migration and returns the data
// directory to its pre-migration state. When the cutover never published,
// the original state file is still in place and only migration artifacts
// are removed; when it did, the verified pre-migration backup is restored.
func Rollback(dataDir string) (*RollbackReport, error) {
	interrupted, err := journalExists(dataDir)
	if err != nil {
		return nil, err
	}
	if !interrupted {
		report := &RollbackReport{Action: "none"}
		raw, err := os.ReadFile(statePath(dataDir))
		if errors.Is(err, os.ErrNotExist) {
			return report, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read state: %w", err)
		}
		_, version, err := DecodeState(raw)
		if err != nil {
			return nil, err
		}
		report.DataVersion = version
		return report, nil
	}

	journal, err := readJournal(dataDir)
	if err != nil {
		return nil, err
	}
	report := &RollbackReport{
		MigrationID: journal.MigrationID,
		BackupFile:  journal.BackupFile,
	}

	raw, err := os.ReadFile(statePath(dataDir))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := restoreBackup(dataDir, journal); err != nil {
			return nil, err
		}
		report.Action = "restored"
		report.DataVersion = journal.FromVersion
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	default:
		_, version, detectErr := DecodeState(raw)
		if detectErr != nil {
			return nil, detectErr
		}
		switch version {
		case journal.FromVersion:
			// The cutover never published; the original data is intact.
			report.Action = "discarded"
			report.DataVersion = version
		case journal.ToVersion:
			if err := restoreBackup(dataDir, journal); err != nil {
				return nil, err
			}
			report.Action = "restored"
			report.DataVersion = journal.FromVersion
		default:
			return nil, fmt.Errorf(
				"state file is at version %d, outside the interrupted migration range %d -> %d; refusing to roll back",
				version, journal.FromVersion, journal.ToVersion)
		}
	}

	if err := removeIfExists(stagingPath(dataDir)); err != nil {
		return nil, err
	}
	if err := removeIfExists(journalPath(dataDir)); err != nil {
		return nil, err
	}
	if err := syncDir(dataDir); err != nil {
		return nil, err
	}
	return report, nil
}

// restoreBackup verifies the pre-migration backup and atomically puts it
// back as the live state file.
func restoreBackup(dataDir string, journal Journal) error {
	backupPath := filepath.Join(dataDir, journal.BackupFile)
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read pre-migration backup: %w", err)
	}
	_, version, err := DecodeState(raw)
	if err != nil {
		return fmt.Errorf("pre-migration backup is not usable: %w", err)
	}
	if version != journal.FromVersion {
		return fmt.Errorf("pre-migration backup is version %d, expected version %d",
			version, journal.FromVersion)
	}
	if err := writeFileAtomic(statePath(dataDir), raw); err != nil {
		return fmt.Errorf("restore pre-migration backup: %w", err)
	}
	return nil
}

// syncDir commits directory entries (renames, removals) to durable storage
// so migration phase transitions survive power loss.
func syncDir(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open data directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("commit data directory: %w", err)
	}
	return nil
}
