// Package migrate owns the versioned on-disk schema of the cellar state
// file: version detection, checksummed envelopes, the approved migration
// registry, and the crash-safe migration journal.
//
// The live state file is only ever replaced by a complete, verified
// snapshot at a known schema version. A migration builds its result in a
// staging file and atomically renames it over the state file as the last
// step, so readers never observe a partially migrated database.
package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

const (
	// LegacyVersion is the schema version of snapshots written before
	// versioned envelopes existed: a bare domain.Snapshot JSON document.
	LegacyVersion = 1

	// CurrentVersion is the newest schema version this binary can read,
	// write, and migrate to.
	CurrentVersion = 2

	// producerID identifies state files written by this service. A file
	// that carries an envelope but a different producer is refused.
	producerID = "cellar-run-control"
)

var (
	// ErrUnknownOrigin is returned when a state file is neither a
	// recognizable legacy snapshot nor an envelope written by this service.
	ErrUnknownOrigin = errors.New("data origin is not recognized")

	// ErrFutureVersion is returned when a state file was written by a newer
	// schema version than this binary understands.
	ErrFutureVersion = errors.New("data version is newer than this binary")

	// ErrChecksumMismatch is returned when an envelope checksum does not
	// match its snapshot payload.
	ErrChecksumMismatch = errors.New("state checksum does not match its contents")

	// ErrNoMigrationPath is returned when no approved migration chain
	// reaches the current version.
	ErrNoMigrationPath = errors.New("no approved migration path")
)

// envelope is the on-disk framing for schema version 2 and later. The
// checksum covers the exact stored snapshot bytes, so verification never
// depends on re-encoding.
type envelope struct {
	Producer      string          `json:"producer"`
	SchemaVersion int             `json:"schema_version"`
	Checksum      string          `json:"checksum"`
	Snapshot      json.RawMessage `json:"snapshot"`
}

func checksumOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// payloadChecksum hashes the canonical compact form of a snapshot payload.
// The envelope is stored indented for operator readability, so verification
// always canonicalizes before hashing; compaction and indentation are exact
// inverses for the whitespace they touch.
func payloadChecksum(raw json.RawMessage) (string, error) {
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return "", fmt.Errorf("canonicalize snapshot payload: %w", err)
	}
	return checksumOf(compacted.Bytes()), nil
}

// encodeEnvelope frames the snapshot at the given schema version and
// returns the encoded document along with the payload checksum recorded in
// it.
func encodeEnvelope(snapshot domain.Snapshot, version int) ([]byte, string, error) {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, "", fmt.Errorf("encode snapshot payload: %w", err)
	}
	checksum := checksumOf(payload)
	framed := envelope{
		Producer:      producerID,
		SchemaVersion: version,
		Checksum:      checksum,
		Snapshot:      payload,
	}
	encoded, err := json.MarshalIndent(framed, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("encode state envelope: %w", err)
	}
	return append(encoded, '\n'), checksum, nil
}

// WriteState durably writes the snapshot as a checksummed envelope at the
// current schema version, replacing any previous file atomically.
func WriteState(path string, snapshot domain.Snapshot) error {
	encoded, _, err := encodeEnvelope(snapshot, CurrentVersion)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, encoded)
}

// ReadState decodes a state file and reports its schema version. It refuses
// files of unknown origin, files from the future, and envelopes whose
// checksum does not verify.
func ReadState(path string) (domain.Snapshot, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Snapshot{}, 0, fmt.Errorf("read state: %w", err)
	}
	return DecodeState(raw)
}

// DecodeState decodes state file bytes and reports the schema version.
func DecodeState(raw []byte) (domain.Snapshot, int, error) {
	var probe struct {
		Producer      *string `json:"producer"`
		SchemaVersion *int    `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return domain.Snapshot{}, 0, fmt.Errorf("decode state: %w", err)
	}
	if probe.SchemaVersion == nil {
		return decodeLegacy(raw)
	}
	return decodeEnvelope(raw, probe)
}

// decodeLegacy accepts only JSON documents that carry the known snapshot
// collections; anything else is of unknown origin.
func decodeLegacy(raw []byte) (domain.Snapshot, int, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return domain.Snapshot{}, 0, fmt.Errorf("decode state: %w", err)
	}
	for _, required := range []string{"revision", "formulas", "vessels", "runs"} {
		if _, found := keys[required]; !found {
			return domain.Snapshot{}, 0, fmt.Errorf(
				"%w: state file has no schema version and is missing %q",
				ErrUnknownOrigin, required)
		}
	}
	var snapshot domain.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return domain.Snapshot{}, 0, fmt.Errorf("decode legacy state: %w", err)
	}
	return snapshot, LegacyVersion, nil
}

func decodeEnvelope(raw []byte, probe struct {
	Producer      *string `json:"producer"`
	SchemaVersion *int    `json:"schema_version"`
}) (domain.Snapshot, int, error) {
	if probe.Producer == nil || *probe.Producer != producerID {
		return domain.Snapshot{}, 0, fmt.Errorf(
			"%w: state envelope was not written by %s", ErrUnknownOrigin, producerID)
	}
	version := *probe.SchemaVersion
	if version > CurrentVersion {
		return domain.Snapshot{}, 0, fmt.Errorf(
			"%w: state is version %d, this binary understands up to version %d",
			ErrFutureVersion, version, CurrentVersion)
	}
	if version <= LegacyVersion {
		return domain.Snapshot{}, 0, fmt.Errorf(
			"%w: state envelope claims unsupported version %d", ErrUnknownOrigin, version)
	}
	var framed envelope
	if err := json.Unmarshal(raw, &framed); err != nil {
		return domain.Snapshot{}, 0, fmt.Errorf("decode state envelope: %w", err)
	}
	if framed.Checksum == "" || len(framed.Snapshot) == 0 {
		return domain.Snapshot{}, 0, fmt.Errorf(
			"%w: state envelope is incomplete", ErrUnknownOrigin)
	}
	actual, err := payloadChecksum(framed.Snapshot)
	if err != nil {
		return domain.Snapshot{}, 0, err
	}
	if actual != framed.Checksum {
		return domain.Snapshot{}, 0, fmt.Errorf(
			"%w: expected %s, computed %s", ErrChecksumMismatch, framed.Checksum, actual)
	}
	var snapshot domain.Snapshot
	if err := json.Unmarshal(framed.Snapshot, &snapshot); err != nil {
		return domain.Snapshot{}, 0, fmt.Errorf("decode enveloped snapshot: %w", err)
	}
	return snapshot, version, nil
}
