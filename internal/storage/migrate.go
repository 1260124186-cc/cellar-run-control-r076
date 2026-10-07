package storage

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// migration is one approved, ordered schema transformation. Only
// migrations reviewed and registered here may run; the engine never
// derives a migration path from the data itself.
type migration struct {
	Version int
	Name    string
	Apply   func(domain.Snapshot) (domain.Snapshot, error)
}

// approvedMigrations is the ordered chain of vetted migrations. Each entry
// upgrades the persisted state from Version-1 to Version.
var approvedMigrations = []migration{
	{
		Version: 2,
		Name:    "formula-ingredients-and-version-refs",
		Apply:   migrateV1ToV2,
	},
}

// migrateV1ToV2 moves formulas from the single step list to the
// ingredient-plus-version-reference layout. Every step receives a
// deterministic reference to the legacy template it was derived from, and
// each formula gains an empty ingredient list for the planner to fill in.
// The transformation is deterministic so an interrupted migration can be
// replayed from the original bytes.
func migrateV1ToV2(snapshot domain.Snapshot) (domain.Snapshot, error) {
	migrated := snapshot.Clone()
	for id, formula := range migrated.Formulas {
		if formula.Ingredients == nil {
			formula.Ingredients = []domain.FormulaIngredient{}
		}
		for index := range formula.Steps {
			step := &formula.Steps[index]
			if step.TemplateRef == "" {
				step.TemplateRef = legacyTemplateRef(step.Sequence, step.Label)
			}
			if step.TemplateVersion == 0 {
				step.TemplateVersion = 1
			}
		}
		migrated.Formulas[id] = formula
	}
	return migrated, nil
}

// legacyTemplateRef derives a stable template reference for a migrated
// step from its label, falling back to the sequence number when the label
// carries no usable characters.
func legacyTemplateRef(sequence int, label string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "-") {
				builder.WriteByte('-')
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return fmt.Sprintf("legacy/step-%d", sequence)
	}
	return "legacy/" + slug
}

// pendingMigrations returns the approved chain leading from version to the
// current schema version, or an error when no approved path exists.
func pendingMigrations(version int) ([]migration, error) {
	if version < LegacySchemaVersion {
		return nil, fmt.Errorf(
			"state schema version %d is not a recognized version; refusing to migrate", version)
	}
	var chain []migration
	for _, step := range approvedMigrations {
		if step.Version > version && step.Version <= CurrentSchemaVersion {
			chain = append(chain, step)
		}
	}
	if version+len(chain) != CurrentSchemaVersion {
		return nil, fmt.Errorf(
			"no approved migration path from schema version %d to %d; refusing to migrate",
			version, CurrentSchemaVersion)
	}
	return chain, nil
}

// Migrate upgrades the data directory to the current schema version. It is
// idempotent: a directory already at the current version is left
// untouched. An interrupted previous attempt is resumed from its journal;
// the original state bytes are preserved until the final atomic commit, so
// a failed migration always leaves the pre-migration data usable.
func Migrate(dir string) (*MigrationReport, error) {
	journal, err := readJournal(dir)
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(dir, stateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			if journal != nil {
				return nil, fmt.Errorf(
					"migration journal is present but %s is missing; restore the state file or the backup before continuing",
					stateFileName)
			}
			return &MigrationReport{Performed: false, ToVersion: CurrentSchemaVersion}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	state, err := decodeState(raw)
	if err != nil {
		return nil, err
	}

	if journal != nil {
		switch state.Version {
		case journal.ToVersion:
			// The commit completed but the journal cleanup was
			// interrupted. The state is already whole; finalize the
			// record and continue.
			report := journal.report(time.Now().UTC())
			if err := writeReport(dir, report); err != nil {
				return nil, err
			}
			if err := removeJournal(dir); err != nil {
				return nil, err
			}
			if state.Version == CurrentSchemaVersion {
				return &report, nil
			}
		case journal.FromVersion:
			// The previous attempt was interrupted before the commit.
			// The state still holds the original bytes; replay the
			// deterministic transformations from the top.
			journal.Resumed = true
			journal.ToVersion = CurrentSchemaVersion
			return runMigration(dir, state, journal)
		default:
			return nil, fmt.Errorf(
				"migration journal covers versions %d to %d but the state file is at version %d; refusing to continue",
				journal.FromVersion, journal.ToVersion, state.Version)
		}
	}

	if state.Version == CurrentSchemaVersion {
		return &MigrationReport{Performed: false, ToVersion: CurrentSchemaVersion}, nil
	}
	if _, err := pendingMigrations(state.Version); err != nil {
		return nil, err
	}
	if err := backupState(dir, raw, state.Version); err != nil {
		return nil, err
	}
	return runMigration(dir, state, newJournal(state.Version))
}

// runMigration applies the pending chain in order, records every step in
// the journal, and commits the result with one atomic replacement.
func runMigration(dir string, state versionedState, journal *migrationJournal) (*MigrationReport, error) {
	chain, err := pendingMigrations(state.Version)
	if err != nil {
		return nil, err
	}
	// Rebuild the step records from the approved chain: a resumed journal
	// may carry stale records from the interrupted attempt.
	journal.Steps = make([]migrationStepRecord, 0, len(chain))
	for _, step := range chain {
		journal.Steps = append(journal.Steps, migrationStepRecord{
			Version: step.Version,
			Name:    step.Name,
			Status:  stepPending,
		})
	}
	if err := writeJournal(dir, journal); err != nil {
		return nil, err
	}

	// Prove the source data is coherent before transforming it, so a
	// corrupt pre-migration file is reported as such rather than as a
	// failed migration product.
	if err := validateMigratedSnapshot(state.Snapshot); err != nil {
		return nil, fmt.Errorf("state at schema version %d is not valid: %w",
			state.Version, err)
	}

	working := state.Snapshot
	for index, step := range chain {
		record := &journal.Steps[index]
		preChecksum, err := checksumSnapshot(working)
		if err != nil {
			return nil, err
		}
		upgraded, err := step.Apply(working)
		if err != nil {
			return nil, fmt.Errorf("apply migration %q: %w", step.Name, err)
		}
		if err := validateMigratedSnapshot(upgraded); err != nil {
			return nil, fmt.Errorf("migration %q produced invalid state: %w", step.Name, err)
		}
		postChecksum, err := checksumSnapshot(upgraded)
		if err != nil {
			return nil, err
		}
		appliedAt := time.Now().UTC()
		record.Status = stepApplied
		record.PreChecksum = preChecksum
		record.PostChecksum = postChecksum
		record.Formulas = len(upgraded.Formulas)
		record.Vessels = len(upgraded.Vessels)
		record.Runs = len(upgraded.Runs)
		record.Validated = true
		record.AppliedAt = &appliedAt
		if err := writeJournal(dir, journal); err != nil {
			return nil, err
		}
		log.Printf("migration step %d/%d applied: %s (schema v%d, formulas=%d vessels=%d runs=%d)",
			index+1, len(chain), step.Name, step.Version,
			record.Formulas, record.Vessels, record.Runs)
		working = upgraded
	}

	encoded, err := encodeState(working)
	if err != nil {
		return nil, err
	}
	if err := writeBytesAtomic(filepath.Join(dir, stateFileName), encoded); err != nil {
		return nil, fmt.Errorf("commit migrated state: %w", err)
	}
	journal.Status = journalCommitted
	if err := writeJournal(dir, journal); err != nil {
		return nil, err
	}
	report := journal.report(time.Now().UTC())
	if err := writeReport(dir, report); err != nil {
		return nil, err
	}
	if err := removeJournal(dir); err != nil {
		return nil, err
	}
	log.Printf("migration committed: schema v%d -> v%d", report.FromVersion, report.ToVersion)
	return &report, nil
}

// validateMigratedSnapshot proves a migration step produced a coherent
// snapshot: consistent indexes and domain-valid entities.
func validateMigratedSnapshot(snapshot domain.Snapshot) error {
	normalized, err := normalizeSnapshot(snapshot)
	if err != nil {
		return err
	}
	for id, formula := range normalized.Formulas {
		if err := formula.Validate(); err != nil {
			return fmt.Errorf("formula %s: %w", id, err)
		}
	}
	for id, vessel := range normalized.Vessels {
		if err := vessel.Validate(); err != nil {
			return fmt.Errorf("vessel %s: %w", id, err)
		}
	}
	for id, run := range normalized.Runs {
		if err := run.Validate(); err != nil {
			return fmt.Errorf("run %s: %w", id, err)
		}
	}
	return nil
}

// backupState preserves the pre-migration state bytes under backups/ so an
// operator can roll back to the original data. An existing backup of the
// same version is kept: it belongs to the first, authoritative attempt.
func backupState(dir string, raw []byte, version int) error {
	backupDir := filepath.Join(dir, backupDirectoryName)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	path := filepath.Join(backupDir, fmt.Sprintf("state-v%d.json", version))
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := writeBytesAtomic(path, raw); err != nil {
		return fmt.Errorf("persist pre-migration backup: %w", err)
	}
	return nil
}

// MigrationStatus describes the versioned state of a data directory for
// operator inspection.
type MigrationStatus struct {
	StateExists    bool             `json:"state_exists"`
	StateVersion   int              `json:"state_version"`
	CurrentVersion int              `json:"current_version"`
	JournalActive  bool             `json:"journal_active"`
	JournalFrom    int              `json:"journal_from_version,omitempty"`
	JournalTo      int              `json:"journal_to_version,omitempty"`
	JournalStatus  string           `json:"journal_status,omitempty"`
	LastReport     *MigrationReport `json:"last_report,omitempty"`
}

// InspectMigration reports the persisted schema version, any in-flight
// migration journal, and the last completed migration report without
// changing anything on disk.
func InspectMigration(dir string) (*MigrationStatus, error) {
	status := &MigrationStatus{CurrentVersion: CurrentSchemaVersion}
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	switch {
	case err == nil:
		state, err := decodeState(raw)
		if err != nil {
			return nil, err
		}
		status.StateExists = true
		status.StateVersion = state.Version
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("read state: %w", err)
	}
	journal, err := readJournal(dir)
	if err != nil {
		return nil, err
	}
	if journal != nil {
		status.JournalActive = true
		status.JournalFrom = journal.FromVersion
		status.JournalTo = journal.ToVersion
		status.JournalStatus = journal.Status
	}
	report, err := ReadMigrationReport(dir)
	if err != nil {
		return nil, err
	}
	status.LastReport = report
	return status, nil
}

// RollbackReport describes the outcome of a rollback.
type RollbackReport struct {
	RestoredVersion int  `json:"restored_version"`
	RestoredBackup  bool `json:"restored_backup"`
}

// RollbackMigration abandons an interrupted migration and returns the data
// directory to the pre-migration bytes. It refuses to run when no
// migration is in flight, so a committed, serving state is never
// overwritten silently.
func RollbackMigration(dir string) (*RollbackReport, error) {
	journal, err := readJournal(dir)
	if err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, fmt.Errorf("no interrupted migration to roll back")
	}
	statePath := filepath.Join(dir, stateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	state, err := decodeState(raw)
	if err != nil {
		return nil, err
	}
	report := &RollbackReport{RestoredVersion: journal.FromVersion}
	switch state.Version {
	case journal.FromVersion:
		// The commit never happened; the original data is already in
		// place. Only the journal needs to go.
	case journal.ToVersion:
		backup, err := os.ReadFile(filepath.Join(dir, backupDirectoryName,
			fmt.Sprintf("state-v%d.json", journal.FromVersion)))
		if err != nil {
			return nil, fmt.Errorf("read pre-migration backup: %w", err)
		}
		if _, err := decodeState(backup); err != nil {
			return nil, fmt.Errorf("pre-migration backup is not usable: %w", err)
		}
		if err := writeBytesAtomic(statePath, backup); err != nil {
			return nil, fmt.Errorf("restore pre-migration state: %w", err)
		}
		report.RestoredBackup = true
	default:
		return nil, fmt.Errorf(
			"state file is at version %d, outside the interrupted migration range %d to %d; refusing to roll back",
			state.Version, journal.FromVersion, journal.ToVersion)
	}
	if err := removeJournal(dir); err != nil {
		return nil, err
	}
	if err := removeReport(dir); err != nil {
		return nil, err
	}
	return report, nil
}
