package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// JournalFormat is the provenance marker of a migration journal.
const JournalFormat = "cellar-run-control/migration-journal"

// Journal and step statuses.
const (
	journalInProgress = "in_progress"
	journalCommitted  = "committed"

	stepPending = "pending"
	stepApplied = "applied"
)

// migrationStepRecord is the checkable result of one applied migration:
// what changed, how many records it touched, and the integrity tokens
// before and after the transformation.
type migrationStepRecord struct {
	Version      int        `json:"version"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	PreChecksum  string     `json:"pre_checksum"`
	PostChecksum string     `json:"post_checksum"`
	Formulas     int        `json:"formulas"`
	Vessels      int        `json:"vessels"`
	Runs         int        `json:"runs"`
	Validated    bool       `json:"validated"`
	AppliedAt    *time.Time `json:"applied_at,omitempty"`
}

// migrationJournal tracks an in-flight migration so a power loss can be
// resumed or rolled back cleanly. The journal is fsynced after every
// change and removed only after the migrated state is committed.
type migrationJournal struct {
	Format      string                `json:"format"`
	FromVersion int                   `json:"from_version"`
	ToVersion   int                   `json:"to_version"`
	Status      string                `json:"status"`
	StartedAt   time.Time             `json:"started_at"`
	Resumed     bool                  `json:"resumed,omitempty"`
	Steps       []migrationStepRecord `json:"steps"`
}

// MigrationReport is the durable, inspectable outcome of a migration run.
// It is persisted as migration-report.json next to the state file.
type MigrationReport struct {
	Performed   bool                  `json:"performed"`
	FromVersion int                   `json:"from_version"`
	ToVersion   int                   `json:"to_version"`
	Resumed     bool                  `json:"resumed,omitempty"`
	StartedAt   time.Time             `json:"started_at,omitempty"`
	FinishedAt  time.Time             `json:"finished_at,omitempty"`
	Steps       []migrationStepRecord `json:"steps,omitempty"`
}

func newJournal(fromVersion int) *migrationJournal {
	return &migrationJournal{
		Format:      JournalFormat,
		FromVersion: fromVersion,
		ToVersion:   CurrentSchemaVersion,
		Status:      journalInProgress,
		StartedAt:   time.Now().UTC(),
		Steps:       []migrationStepRecord{},
	}
}

func (j *migrationJournal) report(finishedAt time.Time) MigrationReport {
	return MigrationReport{
		Performed:   true,
		FromVersion: j.FromVersion,
		ToVersion:   j.ToVersion,
		Resumed:     j.Resumed,
		StartedAt:   j.StartedAt,
		FinishedAt:  finishedAt,
		Steps:       append([]migrationStepRecord(nil), j.Steps...),
	}
}

func readJournal(dir string) (*migrationJournal, error) {
	raw, err := os.ReadFile(filepath.Join(dir, journalFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read migration journal: %w", err)
	}
	var journal migrationJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return nil, fmt.Errorf("decode migration journal: %w", err)
	}
	if journal.Format != JournalFormat {
		return nil, fmt.Errorf("migration journal has an unrecognized format marker")
	}
	if journal.Status != journalInProgress && journal.Status != journalCommitted {
		return nil, fmt.Errorf("migration journal has an unknown status %q", journal.Status)
	}
	return &journal, nil
}

func writeJournal(dir string, journal *migrationJournal) error {
	if err := writeSnapshot(filepath.Join(dir, journalFileName), journal); err != nil {
		return fmt.Errorf("persist migration journal: %w", err)
	}
	return nil
}

func removeJournal(dir string) error {
	path := filepath.Join(dir, journalFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove migration journal: %w", err)
	}
	return syncDir(dir)
}

// removeReport drops the completion record of a migration whose effects
// were rolled back, so status output never describes undone work.
func removeReport(dir string) error {
	path := filepath.Join(dir, reportFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove migration report: %w", err)
	}
	return syncDir(dir)
}

func writeReport(dir string, report MigrationReport) error {
	if err := writeSnapshot(filepath.Join(dir, reportFileName), report); err != nil {
		return fmt.Errorf("persist migration report: %w", err)
	}
	return nil
}

// ReadMigrationReport loads the report of the last completed migration,
// or nil when no migration has completed on this data directory.
func ReadMigrationReport(dir string) (*MigrationReport, error) {
	raw, err := os.ReadFile(filepath.Join(dir, reportFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read migration report: %w", err)
	}
	var report MigrationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("decode migration report: %w", err)
	}
	return &report, nil
}
