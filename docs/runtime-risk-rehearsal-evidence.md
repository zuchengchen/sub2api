# Runtime Risk Cutover Rehearsal Evidence

This record is for the isolated rehearsal required by the runtime
customizations migration goal. It contains no credentials, request bodies, or
operator secrets.

## Current Run

- Run date: 2026-08-13 UTC
- Branch: `feature/migrate-runtime-customizations`
- Source reference: `55a04101365d6001b28b3f43541cd9be60fbd3a7`
- Evidence directory: `/tmp/sub2api-runtime-risk-final.SrlieE`
- Reference old binary SHA-256: `34d6b28105a5d5cce0e216b47902048edf006ff3ca85c61148771de1d2c27634`
- Rebuilt new binary SHA-256: `d81375ecc6736086e22987f40120cc9bc3d780a7e327a29514cbe9b245a0ba8d`
- Rebuilt migration binary SHA-256: `1a64c467bcb0f017291bc025fbafbd98b6110e161518288efc937643254d721c`

## Result

`rehearsal-result.json` from the final isolated run reported `status: passed`.
The run verified:

- backup listing and restore, repeatable online preparation, and final delta;
- HTTP 503 with `Retry-After: 30` during maintenance;
- immediate HTTP, SSE, and WebSocket termination (`8 ms` observed);
- all health, finalized-migration, key-ring, private-directory, and queue-lock gates;
- exactly one new-service start and a `1443 ms` forward window, below 30 seconds;
- restoration of legacy tables and configuration without removing unified archive
  columns or migrated rows; and
- old-binary health and read/write smoke behavior after rollback.

The rehearsal was run by:

```bash
/bin/bash deploy/tests/runtime-risk-isolated-rehearsal.sh
```

It used random loopback ports, temporary Docker PostgreSQL/Redis containers,
the recorded old binary, freshly built feature-branch binaries, and the local
connection stub. No production service, database, or Nginx instance was used.

The named functional and migration acceptance tests are recorded separately in
their fresh structured `go test -json` evidence files during final validation.
