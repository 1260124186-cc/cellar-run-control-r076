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

const (
	stateFileName   = "state.json"
	journalFileName = "migration.journal"
	stagingFileName = "migration.staging"
	backupDirName   = "backups"
)

// StepStatus describes where one migration step stands inside a journal.
type StepStatus string

const (
	StepPending  StepStatus = "pending"
	StepVerified StepStatus = "verified"
	StepFailed   StepStatus = "failed"
)

// StepRecord is the durable, inspectable result of one migration step.
type StepRecord struct {
	Name        string     `json:"name"`
	FromVersion int        `json:"from_version"`
	ToVersion   int        `json:"to_version"`
	Status      StepStatus `json:"status"`
	Checksum    string     `json:"checksum,omitempty"`
	Error       string     `json:"error,omitempty"`
	AppliedAt   *time.Time `json:"applied_at,omitempty"`
}

// Journal tracks an in-progress migration so an interrupted upgrade can be
// resumed or rolled back. The journal file exists only while a migration is
// incomplete.
type Journal struct {
	Producer    string       `json:"producer"`
	MigrationID string       `json:"migration_id"`
	FromVersion int          `json:"from_version"`
	ToVersion   int          `json:"to_version"`
	BackupFile  string       `json:"backup_file"`
	Steps       []StepRecord `json:"steps"`
	StartedAt   time.Time    `json:"started_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func statePath(dataDir string) string   { return filepath.Join(dataDir, stateFileName) }
func journalPath(dataDir string) string { return filepath.Join(dataDir, journalFileName) }
func stagingPath(dataDir string) string { return filepath.Join(dataDir, stagingFileName) }

func newJournal(from, to int, steps []Migration, backupFile string) (Journal, error) {
	id, err := newMigrationID()
	if err != nil {
		return Journal{}, err
	}
	now := time.Now().UTC()
	records := make([]StepRecord, len(steps))
	for index, step := range steps {
		records[index] = StepRecord{
			Name:        step.Name,
			FromVersion: step.FromVersion,
			ToVersion:   step.ToVersion,
			Status:      StepPending,
		}
	}
	return Journal{
		Producer:    producerID,
		MigrationID: id,
		FromVersion: from,
		ToVersion:   to,
		BackupFile:  backupFile,
		Steps:       records,
		StartedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func newMigrationID() (string, error) {
	id, err := domain.NewID("migration")
	if err != nil {
		return "", fmt.Errorf("generate migration identifier: %w", err)
	}
	return id, nil
}

// firstPending returns the index of the first step that still needs work,
// or len(Steps) when every step is verified.
func (j Journal) firstPending() int {
	for index, step := range j.Steps {
		if step.Status != StepVerified {
			return index
		}
	}
	return len(j.Steps)
}

func writeJournal(dataDir string, journal Journal) error {
	journal.UpdatedAt = time.Now().UTC()
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode migration journal: %w", err)
	}
	return writeFileAtomic(journalPath(dataDir), append(encoded, '\n'))
}

func readJournal(dataDir string) (Journal, error) {
	raw, err := os.ReadFile(journalPath(dataDir))
	if err != nil {
		return Journal{}, fmt.Errorf("read migration journal: %w", err)
	}
	var journal Journal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return Journal{}, fmt.Errorf("decode migration journal: %w", err)
	}
	if err := journal.validate(); err != nil {
		return Journal{}, err
	}
	return journal, nil
}

// journalExists reports whether an interrupted migration is recorded.
func journalExists(dataDir string) (bool, error) {
	_, err := os.Stat(journalPath(dataDir))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect migration journal: %w", err)
}

// validate checks the journal against the approved registry so a tampered
// or foreign journal cannot drive migration decisions.
func (j Journal) validate() error {
	if j.Producer != producerID {
		return fmt.Errorf("%w: migration journal was not written by %s",
			ErrUnknownOrigin, producerID)
	}
	if j.MigrationID == "" || j.BackupFile == "" {
		return fmt.Errorf("migration journal is incomplete")
	}
	if j.FromVersion >= j.ToVersion || j.ToVersion > CurrentVersion {
		return fmt.Errorf("migration journal records an impossible version range %d -> %d",
			j.FromVersion, j.ToVersion)
	}
	expected, err := plan(j.FromVersion)
	if err != nil {
		return err
	}
	if len(expected) != len(j.Steps) || expected[len(expected)-1].ToVersion != j.ToVersion {
		return fmt.Errorf("migration journal does not match the approved migration chain")
	}
	for index, step := range j.Steps {
		if step.Name != expected[index].Name ||
			step.FromVersion != expected[index].FromVersion ||
			step.ToVersion != expected[index].ToVersion {
			return fmt.Errorf("migration journal step %d does not match the approved chain",
				index+1)
		}
		switch step.Status {
		case StepPending, StepVerified, StepFailed:
		default:
			return fmt.Errorf("migration journal step %q has unknown status %q",
				step.Name, step.Status)
		}
		if step.Status == StepVerified && step.Checksum == "" {
			return fmt.Errorf("migration journal step %q is verified without a checksum",
				step.Name)
		}
	}
	return nil
}
