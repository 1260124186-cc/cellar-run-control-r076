# Cellar Run Control

Cellar Run Control is a local Go HTTP service for coordinating a small
production cellar. It keeps formula approval, vessel availability, active
fermentation runs, gravity observations, conditioning, completion metrics, and
vessel cleaning in one deterministic process.

This repository is a healthy initialization baseline. It has no intentional
defects and no automated test suite. Tests are intentionally deferred to a
later engineering task; the production workflow checks in this repository
remain available for immediate build and behavior verification.

## Requirements

- Go 1.23 or later
- A writable local data directory
- No database, network service, or third-party module

## Build And Run

Build every package:

```bash
go build ./...
```

Start the service:

```bash
go run ./cmd/fermentctl serve
```

The default address is `127.0.0.1:8088`. The default snapshot directory is
`./data`.

Environment variables:

- `CELLAR_CONTROL_ADDR`: HTTP listen address.
- `CELLAR_CONTROL_DATA_DIR`: directory containing `state.json`.

Example:

```bash
CELLAR_CONTROL_ADDR=127.0.0.1:9099 \
CELLAR_CONTROL_DATA_DIR=/tmp/cellar-control \
go run ./cmd/fermentctl serve
```

## Directory Structure

```text
cmd/fermentctl                 service, migration, and workflow-check executable
internal/clock                 time boundary
internal/config                environment configuration
internal/domain                entities, validation, transitions, metrics
internal/httpapi               HTTP routes, JSON decoding, errors
internal/service               use-case orchestration
internal/storage               versioned snapshots, atomic commits, migrations
internal/workflowcheck         real HTTP production checks
```

## Public API

Health:

- `GET /healthz` (reports the snapshot revision and the persisted schema version)

Formula workflow:

- `POST /v1/formulas`
- `GET /v1/formulas`
- `GET /v1/formulas/{id}`
- `PUT /v1/formulas/{id}/steps`
- `PUT /v1/formulas/{id}/ingredients`
- `POST /v1/formulas/{id}/approve`
- `POST /v1/formulas/{id}/retire`

Vessel workflow:

- `POST /v1/vessels`
- `GET /v1/vessels`
- `GET /v1/vessels/{id}`
- `POST /v1/vessels/{id}/finish-cleaning`
- `POST /v1/vessels/{id}/retire`

Run workflow:

- `POST /v1/runs`
- `GET /v1/runs`
- `GET /v1/runs/{id}`
- `POST /v1/runs/{id}/reserve`
- `POST /v1/runs/{id}/start`
- `POST /v1/runs/{id}/observations`
- `POST /v1/runs/{id}/conditioning`
- `POST /v1/runs/{id}/complete`
- `POST /v1/runs/{id}/abort`

Writes accept an optional `X-Expected-Version` header. Errors use a stable
shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "the aggregate version does not match X-Expected-Version",
    "field": ""
  }
}
```

## Workflow Checks

Run each production workflow check separately:

```bash
go run ./cmd/fermentctl check formula-approval
go run ./cmd/fermentctl check vessel-run-lifecycle
go run ./cmd/fermentctl check observations-completion
go run ./cmd/fermentctl check migration
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data. The migration check
additionally plants legacy and interrupted state files to verify upgrades,
refusals, resume, and rollback.

## Persistence And Recovery

The service stores a complete revisioned snapshot at
`<data-directory>/state.json`. A write creates a temporary file in the same
directory, commits the encoded bytes durably, and renames the result over
the previous snapshot; the directory is synced so the rename itself
survives a power loss. The in-memory snapshot is replaced only after that
operation succeeds.

### Schema Versions

The state file is versioned:

- **Version 1 (legacy)**: a bare snapshot document written by pre-upgrade
  binaries. A formula carries a single process step list.
- **Version 2 (current)**: an envelope with a `format` provenance marker,
  the `schema_version`, and a SHA-256 `checksum` of the canonical snapshot
  encoding. Formulas carry an `ingredients` list and steps may carry
  `template_ref` / `template_version` references.

On startup the service identifies the data version before serving:

- A legacy v1 file is migrated to the current version automatically.
- A file whose format marker is not recognized is refused as unknown
  origin.
- A file with a newer schema version than the binary supports is refused.
- A file whose checksum or structural validation fails is refused.

### Migrations

Only migrations registered in the binary (the approved chain) run, in
order. Each applied step is recorded in `migration-journal.json` with its
pre- and post-transformation checksums, record counts, and validation
result, and the journal is fsynced after every step. The original
`state.json` is preserved in `backups/` and is not replaced until every
step has been applied and validated in memory; the final commit is one
atomic rename, so the formula and run APIs never observe partially
migrated data. The completed journal is persisted as
`migration-report.json` for inspection.

A power loss during migration is safe: the next startup finds the journal
and either resumes the migration from the original bytes (the
transformations are deterministic) or finalizes an already-committed
state. An operator can instead roll the directory back to the
pre-migration backup.

Operator commands for a downtime window:

```bash
fermentctl migrate status    # show persisted version, journal, last report
fermentctl migrate run       # execute pending migrations without serving
fermentctl migrate rollback  # abandon an interrupted migration
```

`serve` performs the same migration automatically before it starts
listening, so upgrading during a downtime window is either an explicit
`migrate run` followed by `serve`, or a direct `serve` restart.

## Repair-Ready Boundaries

The baseline intentionally keeps several independent behavior surfaces visible
for later engineering evaluation:

- formula name normalization and duplicate detection;
- target gravity and process-step range validation;
- aggregate version conflict checks;
- atomic vessel reservation and release;
- run state transition guards;
- observation sequence and time ordering;
- gravity movement and range checks;
- metric calculation and rounding;
- durable snapshot replacement and startup normalization.

These are production mechanisms, not injected defects. A later task can add
automated tests or request a focused repair without changing the public API.

## Test Boundary

This stage does not create unit tests, integration tests, test fixtures, or an
E2E suite. The later test task owns systematic coverage of validation limits,
state transitions, concurrency, persistence failures, and numeric edge cases.

## Scope

The service does not control physical equipment, authenticate remote users,
send notifications, expose a browser interface, or replicate data across
processes. It is intended for a trusted local network or an internal adapter.
