package workflowcheck

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

// migrationCheck verifies the versioned migration path end to end: a
// legacy snapshot is upgraded with a checkable report, the served API
// reads only fully migrated data, corrupt or foreign data is refused, and
// an interrupted migration resumes or rolls back cleanly.
func migrationCheck() error {
	if err := migrationUpgradesLegacyState(); err != nil {
		return err
	}
	if err := migrationRefusesUnreadableState(); err != nil {
		return err
	}
	if err := migrationResumeAndRollback(); err != nil {
		return err
	}
	return nil
}

func migrationUpgradesLegacyState() error {
	dir, err := os.MkdirTemp("", "cellar-migration-check-*")
	if err != nil {
		return fmt.Errorf("create migration check directory: %w", err)
	}
	defer os.RemoveAll(dir)

	legacyBytes, err := writeLegacyState(dir)
	if err != nil {
		return err
	}
	store, err := storage.Open(dir)
	if err != nil {
		return fmt.Errorf("open migrated store: %w", err)
	}

	report, err := storage.ReadMigrationReport(dir)
	if err != nil {
		return err
	}
	if report == nil || !report.Performed {
		return fmt.Errorf("migration report was not persisted")
	}
	if report.FromVersion != storage.LegacySchemaVersion ||
		report.ToVersion != storage.CurrentSchemaVersion {
		return fmt.Errorf("migration report covers unexpected versions %d -> %d",
			report.FromVersion, report.ToVersion)
	}
	if len(report.Steps) != 1 || !report.Steps[0].Validated ||
		report.Steps[0].Formulas != 1 || report.Steps[0].Vessels != 1 {
		return fmt.Errorf("migration step record is incomplete: %+v", report.Steps)
	}

	// The committed envelope must carry the checksum the report recorded.
	committed, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return fmt.Errorf("read committed state: %w", err)
	}
	var envelope struct {
		Format        string `json:"format"`
		SchemaVersion int    `json:"schema_version"`
		Checksum      string `json:"checksum"`
	}
	if err := json.Unmarshal(committed, &envelope); err != nil {
		return fmt.Errorf("decode committed envelope: %w", err)
	}
	if envelope.Format != storage.StateFormat ||
		envelope.SchemaVersion != storage.CurrentSchemaVersion {
		return fmt.Errorf("committed state is not a current envelope")
	}
	if envelope.Checksum != report.Steps[0].PostChecksum {
		return fmt.Errorf("committed checksum does not match the migration report")
	}

	// The pre-migration bytes must be preserved for rollback.
	backup, err := os.ReadFile(filepath.Join(dir, "backups", "state-v1.json"))
	if err != nil {
		return fmt.Errorf("read pre-migration backup: %w", err)
	}
	if string(backup) != string(legacyBytes) {
		return fmt.Errorf("pre-migration backup does not match the original state")
	}

	// A second open is a no-op and proves the checksum verifies.
	secondStore, err := storage.Open(dir)
	if err != nil {
		return fmt.Errorf("reopen migrated store: %w", err)
	}
	_ = secondStore

	// The served API exposes the migrated formula with the new fields.
	app := service.New(store, clock.System{})
	server := httptest.NewServer(httpapi.NewHandler(app))
	defer server.Close()
	api := newClient(server.URL)

	health, err := api.call("GET", "/healthz", nil, nil)
	if err != nil {
		return err
	}
	var healthBody struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := health.decode(&healthBody); err != nil {
		return err
	}
	if healthBody.SchemaVersion != storage.CurrentSchemaVersion {
		return fmt.Errorf("health check reports schema version %d", healthBody.SchemaVersion)
	}

	readResponse, err := api.call("GET", "/v1/formulas/formula_legacy01", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(readResponse, 200, "read migrated formula"); err != nil {
		return err
	}
	var formula struct {
		ID          string                     `json:"id"`
		Ingredients []domain.FormulaIngredient `json:"ingredients"`
		Steps       []domain.FormulaStep       `json:"steps"`
	}
	if err := readResponse.decode(&formula); err != nil {
		return err
	}
	if formula.Ingredients == nil || len(formula.Ingredients) != 0 {
		return fmt.Errorf("migrated formula should carry an empty ingredient list")
	}
	if len(formula.Steps) != 2 ||
		formula.Steps[0].TemplateRef != "legacy/heat-and-combine" ||
		formula.Steps[0].TemplateVersion != 1 ||
		formula.Steps[1].TemplateRef != "legacy/hold" {
		return fmt.Errorf("migrated steps miss their version references: %+v", formula.Steps)
	}
	return nil
}

func migrationRefusesUnreadableState() error {
	cases := []struct {
		name    string
		content string
		expect  string
	}{
		{
			name:    "future version",
			content: `{"format":"cellar-run-control/state","schema_version":99,"checksum":"sha256:00","snapshot":{}}`,
			expect:  "newer than the supported version",
		},
		{
			name:    "unknown origin",
			content: `{"hello":"world"}`,
			expect:  "origin is unknown",
		},
		{
			name:    "not json",
			content: `state backup 2026-10-01`,
			expect:  "origin is unknown",
		},
	}
	for _, tc := range cases {
		dir, err := os.MkdirTemp("", "cellar-migration-refuse-*")
		if err != nil {
			return fmt.Errorf("create refusal directory: %w", err)
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(tc.content), 0o644); err != nil {
			return fmt.Errorf("write refusal state: %w", err)
		}
		if _, err := storage.Open(dir); err == nil || !strings.Contains(err.Error(), tc.expect) {
			return fmt.Errorf("%s: expected refusal containing %q, got %v", tc.name, tc.expect, err)
		}
	}

	// A corrupted checksum on an otherwise valid current state must stop
	// the open as well.
	dir, err := os.MkdirTemp("", "cellar-migration-refuse-*")
	if err != nil {
		return fmt.Errorf("create checksum directory: %w", err)
	}
	defer os.RemoveAll(dir)
	store, err := storage.Open(dir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})
	if _, err := app.CreateFormula(service.CreateFormulaInput{
		Name: "Checksum Pale", Style: "Pilot",
		TargetOriginalGravity: 1.052, TargetFinalGravity: 1.012,
		MaxFermentationTempC: 22, MinimumDays: 7, MaximumDays: 21,
	}); err != nil {
		return fmt.Errorf("seed checksum state: %w", err)
	}
	statePath := filepath.Join(dir, "state.json")
	valid, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}
	corrupted := strings.Replace(string(valid), "\"revision\": 1", "\"revision\": 2", 1)
	if corrupted == string(valid) {
		return fmt.Errorf("could not corrupt the state for the checksum check")
	}
	if err := os.WriteFile(statePath, []byte(corrupted), 0o644); err != nil {
		return err
	}
	if _, err := storage.Open(dir); err == nil ||
		!strings.Contains(err.Error(), "integrity check failed") {
		return fmt.Errorf("corrupted checksum: expected integrity refusal, got %v", err)
	}
	return nil
}

func migrationResumeAndRollback() error {
	// A journal left behind by a power loss is resumed: the migration
	// replays from the original bytes and commits.
	dir, err := os.MkdirTemp("", "cellar-migration-resume-*")
	if err != nil {
		return fmt.Errorf("create resume directory: %w", err)
	}
	defer os.RemoveAll(dir)
	if _, err := writeLegacyState(dir); err != nil {
		return err
	}
	if err := fabricateInterruptedJournal(dir); err != nil {
		return err
	}
	if _, err := storage.Open(dir); err != nil {
		return fmt.Errorf("resume interrupted migration: %w", err)
	}
	report, err := storage.ReadMigrationReport(dir)
	if err != nil {
		return err
	}
	if report == nil || !report.Resumed {
		return fmt.Errorf("resumed migration was not recorded as resumed")
	}
	if _, err := os.Stat(filepath.Join(dir, "migration-journal.json")); !os.IsNotExist(err) {
		return fmt.Errorf("journal should be removed after a committed migration")
	}

	// Rolling back before the commit discards the journal and keeps the
	// original bytes untouched.
	rollbackDir, err := os.MkdirTemp("", "cellar-migration-rollback-*")
	if err != nil {
		return fmt.Errorf("create rollback directory: %w", err)
	}
	defer os.RemoveAll(rollbackDir)
	legacyBytes, err := writeLegacyState(rollbackDir)
	if err != nil {
		return err
	}
	if err := fabricateInterruptedJournal(rollbackDir); err != nil {
		return err
	}
	rollback, err := storage.RollbackMigration(rollbackDir)
	if err != nil {
		return fmt.Errorf("roll back interrupted migration: %w", err)
	}
	if rollback.RestoredBackup || rollback.RestoredVersion != storage.LegacySchemaVersion {
		return fmt.Errorf("unexpected rollback outcome: %+v", rollback)
	}
	current, err := os.ReadFile(filepath.Join(rollbackDir, "state.json"))
	if err != nil {
		return err
	}
	if string(current) != string(legacyBytes) {
		return fmt.Errorf("rollback changed the pre-migration state bytes")
	}
	if _, err := os.Stat(filepath.Join(rollbackDir, "migration-journal.json")); !os.IsNotExist(err) {
		return fmt.Errorf("journal should be removed after a rollback")
	}

	// Rolling back after the commit restores the pre-migration backup.
	committedDir, err := os.MkdirTemp("", "cellar-migration-rollback-*")
	if err != nil {
		return fmt.Errorf("create committed rollback directory: %w", err)
	}
	defer os.RemoveAll(committedDir)
	originalBytes, err := writeLegacyState(committedDir)
	if err != nil {
		return err
	}
	if _, err := storage.Migrate(committedDir); err != nil {
		return fmt.Errorf("commit migration for rollback check: %w", err)
	}
	if err := fabricateInterruptedJournal(committedDir); err != nil {
		return err
	}
	restored, err := storage.RollbackMigration(committedDir)
	if err != nil {
		return fmt.Errorf("roll back committed migration: %w", err)
	}
	if !restored.RestoredBackup {
		return fmt.Errorf("committed rollback should restore the backup")
	}
	rolledBack, err := os.ReadFile(filepath.Join(committedDir, "state.json"))
	if err != nil {
		return err
	}
	if string(rolledBack) != string(originalBytes) {
		return fmt.Errorf("committed rollback did not restore the original bytes")
	}
	return nil
}

// writeLegacyState plants a schema version 1 snapshot: a bare snapshot
// document without an envelope, exactly as the pre-upgrade binary wrote
// it. It returns the written bytes for later comparison.
func writeLegacyState(dir string) ([]byte, error) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	formula := domain.Formula{
		ID:                    "formula_legacy01",
		Name:                  "Legacy Pale",
		Style:                 "Pilot",
		TargetOriginalGravity: 1.050,
		TargetFinalGravity:    1.010,
		MaxFermentationTempC:  22,
		MinimumDays:           7,
		MaximumDays:           21,
		Steps: []domain.FormulaStep{
			{Sequence: 1, Label: "Heat and combine", TemperatureC: 66, HoldMinutes: 60},
			{Sequence: 2, Label: "Hold", TemperatureC: 19, HoldMinutes: 240},
		},
		State:      domain.FormulaApproved,
		Version:    3,
		CreatedAt:  now,
		UpdatedAt:  now,
		ApprovedAt: &now,
	}
	vessel := domain.Vessel{
		ID:        "vessel_legacy01",
		Code:      "FV-01",
		CapacityL: 500,
		State:     domain.VesselAvailable,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	snapshot := domain.EmptySnapshot()
	snapshot.Revision = 7
	snapshot.UpdatedAt = now
	snapshot.Formulas[formula.ID] = formula
	snapshot.Vessels[vessel.ID] = vessel
	snapshot.NameIndex[domain.NameKey(formula.Name)] = formula.ID
	snapshot.VesselIndex[vessel.Code] = vessel.ID
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode legacy state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o644); err != nil {
		return nil, fmt.Errorf("write legacy state: %w", err)
	}
	return raw, nil
}

// fabricateInterruptedJournal simulates a power loss after the backup was
// taken but before the migration committed: an in-progress journal and a
// preserved copy of the version 1 state.
func fabricateInterruptedJournal(dir string) error {
	legacy, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return fmt.Errorf("read state for journal fixture: %w", err)
	}
	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(backupDir, "state-v1.json")); os.IsNotExist(err) {
		if err := os.WriteFile(filepath.Join(backupDir, "state-v1.json"), legacy, 0o644); err != nil {
			return fmt.Errorf("write backup fixture: %w", err)
		}
	}
	journal := map[string]any{
		"format":       storage.JournalFormat,
		"from_version": storage.LegacySchemaVersion,
		"to_version":   storage.CurrentSchemaVersion,
		"status":       "in_progress",
		"started_at":   time.Date(2026, 9, 2, 8, 30, 0, 0, time.UTC),
		"steps":        []any{},
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "migration-journal.json"), raw, 0o644); err != nil {
		return fmt.Errorf("write journal fixture: %w", err)
	}
	return nil
}
