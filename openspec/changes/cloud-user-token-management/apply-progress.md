# Apply Progress: Cloud User Token Management

## Current branch

`feat/cloud-user-token-management-storage-auth`

## Chain context

- Tracker branch: `feat/cloud-user-token-management`
- Current slice: PR 1A — `internal/cloud/auth` principal and token foundation
- Out of scope for this slice: cloudstore migrations, server middleware, dashboard, CLI bootstrap, docs beyond this progress file

## Progress

### Completed

- Added `internal/cloud/auth/foundation_test.go` covering:
  - principal kind, role, and source validation/string values;
  - managed token format and entropy shape;
  - dedicated token pepper requirement;
  - domain-separated HMAC token verifier behavior;
  - no raw token material in token verifiers;
  - resolver rejection for revoked tokens and disabled principals;
  - legacy env sync/admin principal resolution.
- Added `internal/cloud/auth/foundation.go` with:
  - principal domain types;
  - MVP roles and principal sources;
  - managed token generation;
  - dedicated pepper HMAC token hasher/verifier;
  - storage-agnostic managed token lookup interface;
  - principal resolver with managed-token and legacy-env resolution.
- Updated `tasks.md` to mark the auth RED/GREEN tasks complete.

### TDD evidence

- RED: A broad PR1 subagent stalled while drafting auth tests before producing a final response; no tracked files were written from that attempt.
- RED/GREEN recovery: A smaller PR1A auth-only implementation produced auth tests and implementation.
- GREEN: `go test ./internal/cloud/auth` passes.
- Review remediation: added resolver guards for token/principal ID mismatch and invalid stored principals; legacy env tokens now resolve before managed-token pepper checks; legacy token comparison hashes both sides before `hmac.Equal` to avoid length-dependent comparison; existing `Service.Authorize` now uses the same legacy comparison helper; token generation tests now use deterministic entropy seams and cover env normalization plus entropy failures; resolver tests now verify backend lookup failures are propagated.

### Validation run

```bash
go test ./internal/cloud/auth
```

Result: PASS.

## Remaining PR 1 work

- Cloudstore migration tests and migrations.
- Cloudstore methods for principals, users, tokens, grants, admin checks, and auth audit insertion.
- Storage-level error-path tests for duplicate usernames/emails, duplicate grants, missing pepper configuration integration, and hash-only persistence.
- Full `go test ./...` after PR 1 is complete.

## Risks

- The first broad subagent stalled and could not be cancelled through `subagent_cancel`; current tracked changes were produced by the smaller recovery slice.
- This is intentionally smaller than the original PR 1 boundary to avoid another oversized/stalled agent run.
