package workflowcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/migrate"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

// legacyStateFixture is a complete schema v1 snapshot as written before
// versioned envelopes existed: no producer, no schema version, no checksum,
// and formula steps without ingredient references.
const legacyStateFixture = `{
  "revision": 7,
  "updated_at": "2026-09-01T10:00:00Z",
  "formulas": {
    "formula_01": {
      "id": "formula_01",
      "name": "Legacy Pale",
      "style": "Cellar pilot",
      "target_original_gravity": 1.052,
      "target_final_gravity": 1.012,
      "max_fermentation_temp_c": 22,
      "minimum_days": 7,
      "maximum_days": 21,
      "steps": [
        {"sequence": 1, "label": "Mash", "temperature_c": 66, "hold_minutes": 60},
        {"sequence": 2, "label": "Ferment", "temperature_c": 19, "hold_minutes": 240}
      ],
      "state": "approved",
      "version": 3,
      "created_at": "2026-09-01T09:00:00Z",
      "updated_at": "2026-09-01T09:30:00Z",
      "approved_at": "2026-09-01T09:30:00Z"
    }
  },
  "vessels": {
    "vessel_01": {
      "id": "vessel_01",
      "code": "VSL-LEGACY",
      "capacity_liters": 240,
      "state": "available",
      "version": 1,
      "created_at": "2026-09-01T09:00:00Z",
      "updated_at": "2026-09-01T09:00:00Z"
    }
  },
  "runs": {
    "run_01": {
      "id": "run_01",
      "code": "RUN-LEGACY01",
      "formula_id": "formula_01",
      "planned_volume_liters": 180,
      "state": "planned",
      "observations": [],
      "version": 1,
      "created_at": "2026-09-01T10:00:00Z",
      "updated_at": "2026-09-01T10:00:00Z"
    }
  },
  "formula_name_index": {"legacy pale": "formula_01"},
  "vessel_code_index": {"VSL-LEGACY": "vessel_01"},
  "run_code_index": {"RUN-LEGACY01": "run_01"}
}
`

const checkBackupName = "backups/state-v1-check.json"

// migrationUpgrade exercises the versioned migration capability: upgrading
// a legacy snapshot, refusing untrusted data, resuming interrupted
// migrations from every crash window, and rolling back cleanly.
func migrationUpgrade() error {
	if err := checkLegacyUpgrade(); err != nil {
		return err
	}
	if err := checkMigrationRefusals(); err != nil {
		return err
	}
	if err := checkMigrationResume(); err != nil {
		return err
	}
	if err := checkMigrationRollback(); err != nil {
		return err
	}
	return nil
}

// checkLegacyUpgrade migrates a v1 snapshot and serves the result over the
// real HTTP API.
func checkLegacyUpgrade() error {
	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	legacy, err := writeLegacyState(dir)
	if err != nil {
		return err
	}
	status := migrate.Inspect(dir)
	if !status.StateExists || status.DataVersion != migrate.LegacyVersion {
		return fmt.Errorf("legacy state was not detected as version %d", migrate.LegacyVersion)
	}
	if len(status.Pending) != 1 {
		return fmt.Errorf("expected one pending migration, found %d", len(status.Pending))
	}

	report, err := migrate.EnsureCurrent(dir)
	if err != nil {
		return fmt.Errorf("migrate legacy state: %w", err)
	}
	if report.InitialVersion != migrate.LegacyVersion ||
		report.FinalVersion != migrate.CurrentVersion || report.Resumed {
		return fmt.Errorf("unexpected migration report %+v", report)
	}
	if len(report.Steps) != 1 || report.Steps[0].Status != migrate.StepVerified ||
		report.Steps[0].Checksum == "" {
		return fmt.Errorf("migration step was not recorded as verified: %+v", report.Steps)
	}
	if report.BackupFile == "" {
		return fmt.Errorf("migration did not record a pre-migration backup")
	}

	migrated, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return err
	}
	if !bytes.Contains(migrated, []byte(`"schema_version": 2`)) {
		return fmt.Errorf("migrated state does not carry schema version 2")
	}
	if !bytes.Contains(migrated, []byte(`"ingredients": []`)) {
		return fmt.Errorf("migrated steps do not carry explicit ingredient lists")
	}
	snapshot, version, err := migrate.ReadState(filepath.Join(dir, "state.json"))
	if err != nil {
		return fmt.Errorf("read migrated state: %w", err)
	}
	if version != migrate.CurrentVersion {
		return fmt.Errorf("migrated state reports version %d", version)
	}
	formula, ok := snapshot.Formulas["formula_01"]
	if !ok || formula.State != domain.FormulaApproved || len(formula.Steps) != 2 {
		return fmt.Errorf("migrated formula data is incomplete")
	}
	for _, step := range formula.Steps {
		if step.Ingredients == nil {
			return fmt.Errorf("migrated step %d has no ingredient list", step.Sequence)
		}
	}
	if snapshot.Revision != 7 {
		return fmt.Errorf("migration changed the business revision to %d", snapshot.Revision)
	}

	backup, err := os.ReadFile(filepath.Join(dir, report.BackupFile))
	if err != nil {
		return fmt.Errorf("read pre-migration backup: %w", err)
	}
	if !bytes.Equal(backup, legacy) {
		return fmt.Errorf("pre-migration backup does not match the original state")
	}
	if _, backupVersion, err := migrate.DecodeState(backup); err != nil || backupVersion != migrate.LegacyVersion {
		return fmt.Errorf("pre-migration backup is not readable legacy data")
	}
	if _, err := os.Stat(filepath.Join(dir, "migration.journal")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("migration journal was not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(dir, "migration.staging")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("migration staging file was not cleaned up")
	}

	status = migrate.Inspect(dir)
	if status.DataVersion != migrate.CurrentVersion || len(status.Pending) != 0 ||
		status.Journal != nil || status.DetectionError != "" {
		return fmt.Errorf("post-migration status is not clean: %+v", status)
	}

	return serveMigratedData(dir)
}

// serveMigratedData opens the migrated directory and drives the public API.
func serveMigratedData(dir string) error {
	store, err := storage.Open(dir)
	if err != nil {
		return fmt.Errorf("open migrated store: %w", err)
	}
	app := service.New(store, clock.System{})
	server := httptest.NewServer(httpapi.NewHandler(app))
	defer server.Close()
	api := newClient(server.URL)

	formulaResponse, err := api.call("GET", "/v1/formulas/formula_01", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(formulaResponse, 200, "read migrated formula"); err != nil {
		return err
	}
	var formula formulaView
	if err := formulaResponse.decode(&formula); err != nil {
		return err
	}
	if formula.State != domain.FormulaApproved || len(formula.Steps) != 2 ||
		formula.Steps[0].Ingredients == nil {
		return fmt.Errorf("migrated formula is not served with ingredient lists")
	}

	runResponse, err := api.call("GET", "/v1/runs/run_01", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(runResponse, 200, "read migrated run"); err != nil {
		return err
	}
	var run runView
	if err := runResponse.decode(&run); err != nil {
		return err
	}
	if run.State != domain.RunPlanned {
		return fmt.Errorf("migrated run changed state to %q", run.State)
	}

	revision, err := healthRevision(api)
	if err != nil {
		return err
	}
	if revision != 7 {
		return fmt.Errorf("expected preserved revision 7, got %d", revision)
	}

	createdResponse, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Post Migration Ale"), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(createdResponse, 201, "create formula after migration"); err != nil {
		return err
	}
	var created formulaView
	if err := createdResponse.decode(&created); err != nil {
		return err
	}
	revision, err = healthRevision(api)
	if err != nil {
		return err
	}
	if revision != 8 {
		return fmt.Errorf("expected revision 8 after a post-migration write, got %d", revision)
	}

	stepsResponse, err := api.call("PUT", "/v1/formulas/"+created.ID+"/steps",
		map[string]any{"steps": []map[string]any{
			{
				"sequence":      1,
				"label":         "Mash with referenced malt",
				"temperature_c": 66.0,
				"hold_minutes":  60,
				"ingredients": []map[string]any{
					{"ingredient_id": "ing_malt_pale", "version": 3},
					{"ingredient_id": "ing_malt_munich", "version": 1},
				},
			},
		}}, ptrInt64(1))
	if err != nil {
		return err
	}
	if err := requireStatus(stepsResponse, 200, "set steps with ingredient refs"); err != nil {
		return err
	}
	var withSteps formulaView
	if err := stepsResponse.decode(&withSteps); err != nil {
		return err
	}
	if len(withSteps.Steps) != 1 || len(withSteps.Steps[0].Ingredients) != 2 ||
		withSteps.Steps[0].Ingredients[0].IngredientID != "ing_malt_pale" ||
		withSteps.Steps[0].Ingredients[0].Version != 3 {
		return fmt.Errorf("ingredient references were not persisted")
	}

	invalidResponse, err := api.call("PUT", "/v1/formulas/"+created.ID+"/steps",
		map[string]any{"steps": []map[string]any{
			{
				"sequence":      1,
				"label":         "Mash with unpinned malt",
				"temperature_c": 66.0,
				"hold_minutes":  60,
				"ingredients":   []map[string]any{{"ingredient_id": "ing_malt_pale", "version": 0}},
			},
		}}, ptrInt64(2))
	if err != nil {
		return err
	}
	if err := requireCode(invalidResponse, 400, string(domain.CodeInvalidInput),
		"reject ingredient ref without a version"); err != nil {
		return err
	}
	return nil
}

// checkMigrationRefusals confirms that untrusted data stops startup and is
// left byte-identical.
func checkMigrationRefusals() error {
	current, err := migrateCurrentFixture()
	if err != nil {
		return err
	}

	future, err := rewriteEnvelopeField(current, "schema_version", json.RawMessage("99"))
	if err != nil {
		return err
	}
	if err := expectRefusal("future", future, migrate.ErrFutureVersion); err != nil {
		return err
	}

	tampered, err := corruptEnvelopePayload(current)
	if err != nil {
		return err
	}
	if err := expectRefusal("checksum", tampered, migrate.ErrChecksumMismatch); err != nil {
		return err
	}

	if err := expectRefusal("unknown keys", []byte(`{"unexpected": true}`),
		migrate.ErrUnknownOrigin); err != nil {
		return err
	}

	foreign, err := rewriteEnvelopeField(current, "producer", json.RawMessage(`"other-system"`))
	if err != nil {
		return err
	}
	if err := expectRefusal("foreign producer", foreign, migrate.ErrUnknownOrigin); err != nil {
		return err
	}

	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	broken := []byte(`{"revision": 7,`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), broken, 0o644); err != nil {
		return err
	}
	if _, err := storage.Open(dir); err == nil {
		return fmt.Errorf("invalid JSON state did not stop startup")
	}
	after, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return err
	}
	if !bytes.Equal(after, broken) {
		return fmt.Errorf("refused state file was modified")
	}
	return nil
}

// expectRefusal writes candidate state bytes and requires storage.Open to
// fail with the given sentinel while leaving the file untouched.
func expectRefusal(label string, contents []byte, sentinel error) error {
	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return err
	}
	if _, err := storage.Open(dir); !errors.Is(err, sentinel) {
		return fmt.Errorf("%s: expected refusal %v, got %v", label, sentinel, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(after, contents) {
		return fmt.Errorf("%s: refused state file was modified", label)
	}
	return nil
}

// checkMigrationResume restarts migrations interrupted at every crash
// window and requires each to finish with the same final bytes.
func checkMigrationResume() error {
	migrated, checksum, err := migrateLegacyFixture()
	if err != nil {
		return err
	}

	// Interrupted before any step was recorded: pending journal, no staging.
	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := writeLegacyState(dir); err != nil {
		return err
	}
	if err := writeCheckBackup(dir); err != nil {
		return err
	}
	if err := writeCheckJournal(dir, migrate.StepPending, ""); err != nil {
		return err
	}
	report, err := migrate.EnsureCurrent(dir)
	if err != nil {
		return fmt.Errorf("resume before first step: %w", err)
	}
	if !report.Resumed || report.FinalVersion != migrate.CurrentVersion {
		return fmt.Errorf("resume before first step did not complete: %+v", report)
	}
	if err := requireStateBytes(dir, migrated); err != nil {
		return fmt.Errorf("resume before first step: %w", err)
	}

	// Interrupted after the step was verified but before cutover: verified
	// journal plus staging, state file still legacy.
	dir, err = scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := writeLegacyState(dir); err != nil {
		return err
	}
	if err := writeCheckBackup(dir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "migration.staging"), migrated, 0o644); err != nil {
		return err
	}
	if err := writeCheckJournal(dir, migrate.StepVerified, checksum); err != nil {
		return err
	}
	report, err = migrate.EnsureCurrent(dir)
	if err != nil {
		return fmt.Errorf("resume before cutover: %w", err)
	}
	if !report.Resumed {
		return fmt.Errorf("resume before cutover was not detected as a resume")
	}
	if err := requireStateBytes(dir, migrated); err != nil {
		return fmt.Errorf("resume before cutover: %w", err)
	}

	// Interrupted after cutover but before journal cleanup: state file
	// already migrated, journal still present.
	dir, err = scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), migrated, 0o644); err != nil {
		return err
	}
	if err := writeCheckBackup(dir); err != nil {
		return err
	}
	if err := writeCheckJournal(dir, migrate.StepVerified, checksum); err != nil {
		return err
	}
	report, err = migrate.EnsureCurrent(dir)
	if err != nil {
		return fmt.Errorf("resume after cutover: %w", err)
	}
	if !report.Resumed || report.FinalVersion != migrate.CurrentVersion {
		return fmt.Errorf("resume after cutover did not finalize: %+v", report)
	}
	if err := requireStateBytes(dir, migrated); err != nil {
		return fmt.Errorf("resume after cutover: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "migration.journal")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("journal survived finalization")
	}
	if _, err := storage.Open(dir); err != nil {
		return fmt.Errorf("open finalized directory: %w", err)
	}
	return nil
}

// checkMigrationRollback confirms both rollback shapes and that a failed
// resume never touches the original data.
func checkMigrationRollback() error {
	migrated, checksum, err := migrateLegacyFixture()
	if err != nil {
		return err
	}

	// Cutover never happened: artifacts are discarded, original bytes stay.
	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	legacy, err := writeLegacyState(dir)
	if err != nil {
		return err
	}
	if err := writeCheckBackup(dir); err != nil {
		return err
	}
	if err := writeCheckJournal(dir, migrate.StepPending, ""); err != nil {
		return err
	}
	rollback, err := migrate.Rollback(dir)
	if err != nil {
		return fmt.Errorf("roll back interrupted migration: %w", err)
	}
	if rollback.Action != "discarded" || rollback.DataVersion != migrate.LegacyVersion {
		return fmt.Errorf("unexpected rollback report %+v", rollback)
	}
	if err := requireStateBytes(dir, legacy); err != nil {
		return fmt.Errorf("rollback modified the original state: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "migration.journal")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("journal survived rollback")
	}
	if _, err := storage.Open(dir); err != nil {
		return fmt.Errorf("re-upgrade after rollback: %w", err)
	}
	if err := requireStateBytes(dir, migrated); err != nil {
		return fmt.Errorf("re-upgrade after rollback: %w", err)
	}

	// Cutover already happened: the pre-migration backup is restored.
	dir, err = scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), migrated, 0o644); err != nil {
		return err
	}
	if err := writeCheckBackup(dir); err != nil {
		return err
	}
	if err := writeCheckJournal(dir, migrate.StepVerified, checksum); err != nil {
		return err
	}
	rollback, err = migrate.Rollback(dir)
	if err != nil {
		return fmt.Errorf("roll back published migration: %w", err)
	}
	if rollback.Action != "restored" || rollback.DataVersion != migrate.LegacyVersion {
		return fmt.Errorf("unexpected restore report %+v", rollback)
	}
	if err := requireStateBytes(dir, legacy); err != nil {
		return fmt.Errorf("restore did not bring back the original state: %w", err)
	}

	// A staging file that does not match its journal record stops the
	// resume, leaves the original bytes untouched, and rolls back cleanly.
	dir, err = scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := writeLegacyState(dir); err != nil {
		return err
	}
	if err := writeCheckBackup(dir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "migration.staging"), migrated, 0o644); err != nil {
		return err
	}
	if err := writeCheckJournal(dir, migrate.StepVerified, "sha256:does-not-match"); err != nil {
		return err
	}
	if _, err := migrate.EnsureCurrent(dir); !errors.Is(err, migrate.ErrChecksumMismatch) {
		return fmt.Errorf("corrupt staging did not stop the resume: %v", err)
	}
	if err := requireStateBytes(dir, legacy); err != nil {
		return fmt.Errorf("failed resume touched the original state: %w", err)
	}
	rollback, err = migrate.Rollback(dir)
	if err != nil {
		return fmt.Errorf("roll back failed resume: %w", err)
	}
	if rollback.Action != "discarded" {
		return fmt.Errorf("unexpected failed-resume rollback %+v", rollback)
	}
	if _, err := storage.Open(dir); err != nil {
		return fmt.Errorf("migrate after failed resume and rollback: %w", err)
	}
	if err := requireStateBytes(dir, migrated); err != nil {
		return fmt.Errorf("migrate after failed resume and rollback: %w", err)
	}

	// Without a journal there is nothing to roll back.
	dir, err = scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := writeLegacyState(dir); err != nil {
		return err
	}
	rollback, err = migrate.Rollback(dir)
	if err != nil {
		return fmt.Errorf("rollback without journal: %w", err)
	}
	if rollback.Action != "none" || rollback.DataVersion != migrate.LegacyVersion {
		return fmt.Errorf("unexpected empty rollback %+v", rollback)
	}
	return nil
}

func scratchDir() (string, error) {
	dir, err := os.MkdirTemp("", "cellar-migration-check-*")
	if err != nil {
		return "", fmt.Errorf("create check directory: %w", err)
	}
	return dir, nil
}

func writeLegacyState(dir string) ([]byte, error) {
	contents := []byte(legacyStateFixture)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), contents, 0o644); err != nil {
		return nil, fmt.Errorf("write legacy state: %w", err)
	}
	return contents, nil
}

// migrateLegacyFixture runs a real migration once so interruption scenarios
// can be staged with authentic artifacts. The encoding is deterministic, so
// every scenario must converge to these exact bytes.
func migrateLegacyFixture() (migrated []byte, checksum string, err error) {
	dir, err := scratchDir()
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	if _, err := writeLegacyState(dir); err != nil {
		return nil, "", err
	}
	if _, err := migrate.EnsureCurrent(dir); err != nil {
		return nil, "", fmt.Errorf("migrate fixture: %w", err)
	}
	migrated, err = os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, "", err
	}
	checksum, err = envelopeChecksum(migrated)
	if err != nil {
		return nil, "", err
	}
	return migrated, checksum, nil
}

// migrateCurrentFixture produces a valid current-version envelope for the
// refusal scenarios to mutate.
func migrateCurrentFixture() ([]byte, error) {
	dir, err := scratchDir()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := migrate.WriteState(filepath.Join(dir, "state.json"), domain.EmptySnapshot()); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "state.json"))
}

func envelopeChecksum(contents []byte) (string, error) {
	var framed struct {
		Checksum string `json:"checksum"`
	}
	if err := json.Unmarshal(contents, &framed); err != nil {
		return "", fmt.Errorf("decode envelope: %w", err)
	}
	if framed.Checksum == "" {
		return "", fmt.Errorf("envelope has no checksum")
	}
	return framed.Checksum, nil
}

// rewriteEnvelopeField returns the envelope with one top-level field
// replaced, keeping every other byte-derived value as is.
func rewriteEnvelopeField(contents []byte, field string, value json.RawMessage) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	document[field] = value
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// corruptEnvelopePayload changes the snapshot payload without updating the
// recorded checksum.
func corruptEnvelopePayload(contents []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(document["snapshot"], &payload); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	payload["revision"] = json.RawMessage("99")
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	document["snapshot"] = encodedPayload
	return json.MarshalIndent(document, "", "  ")
}

// writeCheckBackup stores the legacy fixture under the backup name the
// check journals reference.
func writeCheckBackup(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, checkBackupName), []byte(legacyStateFixture), 0o644)
}

// writeCheckJournal records a one-step journal that matches the approved
// migration chain, as a crashed process would have left it.
func writeCheckJournal(dir string, status migrate.StepStatus, checksum string) error {
	step := migrate.Approved()[0]
	now := time.Now().UTC()
	record := migrate.StepRecord{
		Name:        step.Name,
		FromVersion: step.FromVersion,
		ToVersion:   step.ToVersion,
		Status:      status,
		Checksum:    checksum,
	}
	if status == migrate.StepVerified {
		record.AppliedAt = &now
	}
	journal := migrate.Journal{
		Producer:    "cellar-run-control",
		MigrationID: "migration_checkinterrupted",
		FromVersion: migrate.LegacyVersion,
		ToVersion:   migrate.CurrentVersion,
		BackupFile:  checkBackupName,
		Steps:       []migrate.StepRecord{record},
		StartedAt:   now,
		UpdatedAt:   now,
	}
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "migration.journal"), encoded, 0o644)
}

func requireStateBytes(dir string, expected []byte) error {
	actual, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("state file does not match the expected bytes")
	}
	return nil
}

func healthRevision(api *client) (int64, error) {
	response, err := api.call("GET", "/healthz", nil, nil)
	if err != nil {
		return 0, err
	}
	if err := requireStatus(response, 200, "read health"); err != nil {
		return 0, err
	}
	var health struct {
		Revision int64 `json:"revision"`
	}
	if err := response.decode(&health); err != nil {
		return 0, err
	}
	return health.Revision, nil
}
