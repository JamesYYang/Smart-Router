# Task 1: internal/authkeys Store isolation

## Status

DONE

## Commits

- `bbf8ec9` feat(authkeys): scope Store by explicit tenantID; add isolation test

## Test Summary

`go test ./internal/authkeys/...` — **PASS** (17 passed, 2 skipped [PG/Mongo — no env vars], 0 failed)

## Concerns

None.

---

## Fix Round 1/5 — Admin test mock compilation failure

### Issue

`internal/admin/handler_authkeys_test.go`'s `authKeyTestStore` mock struct implemented
the old `Store` interface without `tenantID` parameters. After P3 Task 1 added `tenantID`
as the first argument to `List`, `Create`, `UpdateLabels`, and `Deactivate`, the mock
no longer satisfied the interface, causing `go test ./internal/admin/...` to fail
compilation.

### Fix

Updated the 4 mock method signatures to accept `_ string` as the first `tenantID`
parameter. No body changes needed — the mock is a simple in-memory map and doesn't
validate tenant isolation.

- `List(_ context.Context)` → `List(_ context.Context, _ string)`
- `Create(_ context.Context, key AuthKey)` → `Create(_ context.Context, _ string, key AuthKey)`
- `UpdateLabels(_ context.Context, id string, ...)` → `UpdateLabels(_ context.Context, _ string, id string, ...)`
- `Deactivate(_ context.Context, id string, ...)` → `Deactivate(_ context.Context, _ string, id string, ...)`

### Verification

- `go test ./internal/admin/...` — PASS
- `go test ./internal/authkeys/...` — PASS (no regression)

### Commit

- `a946a1a` fix(authkeys): update admin test mock to match new Store interface with tenantID params
