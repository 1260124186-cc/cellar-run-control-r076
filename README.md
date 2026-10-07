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
internal/migrate               versioned schema, envelopes, migration journal
internal/service               use-case orchestration
internal/storage               revisioned snapshot persistence
internal/workflowcheck         real HTTP production checks
```

## Public API

Health:

- `GET /healthz`

Formula workflow:

- `POST /v1/formulas`
- `GET /v1/formulas`
- `GET /v1/formulas/{id}`
- `PUT /v1/formulas/{id}/steps`
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

## Schema Migrations

The state file is versioned. The current binary writes schema version 2: a
checksummed envelope whose snapshot carries per-step ingredient references on
formulas. Snapshots written by the previous controller (bare JSON without a
version marker) are schema version 1.

Startup detects the stored version and runs approved migrations in order
before the HTTP port opens, so the upgrade happens inside the downtime
window. Each step verifies its result and records it in a durable journal;
the live state file is replaced atomically only after every step verifies,
and the pre-migration state is kept under `backups/`. Startup refuses data
of unknown origin, data from a future schema version, and data whose
checksum does not verify.

Operator commands (they use the same `CELLAR_CONTROL_DATA_DIR`):

```bash
# inspect stored version, pending migrations, interrupted migrations
go run ./cmd/fermentctl migrate status

# perform pending migrations now, printing each verified step
go run ./cmd/fermentctl migrate run

# abandon an interrupted migration and restore pre-migration data
go run ./cmd/fermentctl migrate rollback
```

If power is lost mid-migration, the next start resumes from the last
verified step; `migrate rollback` instead returns the directory to the
original, still-usable data. Formula and run reads never observe partially
migrated state.

## Workflow Checks

Run each production workflow check separately:

```bash
go run ./cmd/fermentctl check formula-approval
go run ./cmd/fermentctl check vessel-run-lifecycle
go run ./cmd/fermentctl check observations-completion
go run ./cmd/fermentctl check migration-upgrade
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data. The migration check
additionally upgrades a legacy snapshot on disk, interrupts and resumes
migrations at every crash window, rolls interrupted migrations back, and
verifies that unknown, future, or tampered data is refused.

## Persistence And Recovery

The service stores a complete revisioned snapshot at
`<data-directory>/state.json` inside a versioned, checksummed envelope. A
write creates a temporary file in the same directory, commits the encoded
bytes durably, closes the handle, and renames the result over the previous
snapshot. The in-memory snapshot is replaced only after that operation
succeeds.

On startup, an existing snapshot is decoded and normalized. Unknown or
conflicting map indexes are not silently invented; invalid JSON, a negative
revision, an unrecognized producer, a future schema version, or a checksum
mismatch stops the process before HTTP serving begins. See
[Schema Migrations](#schema-migrations) for the upgrade and rollback
protocol.

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
