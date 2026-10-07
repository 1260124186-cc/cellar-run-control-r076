package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// Schema versions of the persisted state.
//
// Version 1 is the legacy layout: a bare domain.Snapshot document with no
// envelope, where a formula carries a single process step list.
//
// Version 2 is the current layout: an envelope with a format marker, the
// schema version, and a SHA-256 checksum of the canonical snapshot
// encoding. Formulas carry an ingredient list and steps may carry
// versioned template references.
const (
	LegacySchemaVersion  = 1
	CurrentSchemaVersion = 2

	// StateFormat is the provenance marker of an envelope state file. A
	// file without this marker is either a legacy snapshot or not produced
	// by this service at all.
	StateFormat = "cellar-run-control/state"

	stateFileName       = "state.json"
	journalFileName     = "migration-journal.json"
	reportFileName      = "migration-report.json"
	backupDirectoryName = "backups"
)

// stateEnvelope is the on-disk layout for CurrentSchemaVersion.
type stateEnvelope struct {
	Format        string          `json:"format"`
	SchemaVersion int             `json:"schema_version"`
	Checksum      string          `json:"checksum"`
	Snapshot      json.RawMessage `json:"snapshot"`
}

// versionedState is the result of classifying a state file.
type versionedState struct {
	Version  int
	Snapshot domain.Snapshot
}

// checksumSnapshot returns the integrity token of a snapshot: the SHA-256
// of its canonical JSON encoding. Go marshals maps with sorted keys, so
// the encoding is deterministic for a given snapshot value.
func checksumSnapshot(snapshot domain.Snapshot) (string, error) {
	canonical, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode snapshot for checksum: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// encodeState renders a snapshot as a versioned, checksummed envelope.
func encodeState(snapshot domain.Snapshot) ([]byte, error) {
	checksum, err := checksumSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	envelope := stateEnvelope{
		Format:        StateFormat,
		SchemaVersion: CurrentSchemaVersion,
		Checksum:      checksum,
		Snapshot:      raw,
	}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode state envelope: %w", err)
	}
	return append(encoded, '\n'), nil
}

// decodeState classifies raw state file bytes and returns the snapshot
// with its schema version. It refuses data whose origin is unknown, whose
// version is newer than this binary, or whose integrity check fails.
func decodeState(raw []byte) (versionedState, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return versionedState{}, fmt.Errorf(
			"state origin is unknown: the state file is not a JSON object")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &keys); err != nil {
		return versionedState{}, fmt.Errorf(
			"state origin is unknown: the state file is not valid JSON: %w", err)
	}
	if _, hasFormat := keys["format"]; hasFormat {
		return decodeEnvelope(trimmed)
	}
	if _, hasRevision := keys["revision"]; hasRevision {
		return decodeLegacy(trimmed)
	}
	return versionedState{}, fmt.Errorf(
		"state origin is unknown: the state file matches neither the envelope nor the legacy layout")
}

func decodeEnvelope(raw []byte) (versionedState, error) {
	var envelope stateEnvelope
	// Unknown envelope fields are tolerated so a newer file reaches the
	// explicit future-version refusal below instead of a decode error;
	// the checksum still pins the snapshot payload itself.
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return versionedState{}, fmt.Errorf("decode state envelope: %w", err)
	}
	if envelope.Format != StateFormat {
		return versionedState{}, fmt.Errorf(
			"state origin is unknown: format marker %q is not recognized", envelope.Format)
	}
	if envelope.SchemaVersion < 1 {
		return versionedState{}, fmt.Errorf(
			"state integrity check failed: schema version %d is not valid", envelope.SchemaVersion)
	}
	if envelope.SchemaVersion > CurrentSchemaVersion {
		return versionedState{}, fmt.Errorf(
			"state schema version %d is newer than the supported version %d; refusing to serve",
			envelope.SchemaVersion, CurrentSchemaVersion)
	}
	if envelope.Checksum == "" || len(envelope.Snapshot) == 0 {
		return versionedState{}, fmt.Errorf(
			"state integrity check failed: the envelope is missing its checksum or snapshot")
	}
	var snapshot domain.Snapshot
	if err := json.Unmarshal(envelope.Snapshot, &snapshot); err != nil {
		return versionedState{}, fmt.Errorf("decode envelope snapshot: %w", err)
	}
	expected, err := checksumSnapshot(snapshot)
	if err != nil {
		return versionedState{}, err
	}
	if expected != envelope.Checksum {
		return versionedState{}, fmt.Errorf(
			"state integrity check failed: the snapshot checksum does not match the envelope")
	}
	return versionedState{Version: envelope.SchemaVersion, Snapshot: snapshot}, nil
}

// decodeLegacy reads a version 1 document: a bare snapshot written before
// the envelope existed. Legacy files predate checksums, so integrity rests
// on the structural validation performed during load.
func decodeLegacy(raw []byte) (versionedState, error) {
	var snapshot domain.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return versionedState{}, fmt.Errorf("decode legacy state: %w", err)
	}
	return versionedState{Version: LegacySchemaVersion, Snapshot: snapshot}, nil
}
