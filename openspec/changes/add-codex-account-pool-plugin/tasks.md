## 1. Plugin Scaffold

- [ ] 1.1 Create `examples/plugin/codex-account-pool/go/go.mod`, the C ABI entry point, lifecycle configuration, and capability registration for Scheduler, UsagePlugin, and ManagementAPI.
- [ ] 1.2 Add the plugin to `examples/plugin/Makefile` and document its configuration and Linux build command.
- [ ] 1.3 Add ABI boundary tests for registration, configuration validation, shutdown, and panic-safe error envelopes.

## 2. Policy and State

- [ ] 2.1 Define validated account policy, route profile, temporary override, quota snapshot, refresh status, and persisted state types.
- [ ] 2.2 Implement atomic `0600` JSON persistence that retains the previous in-memory snapshot when loading or saving fails.
- [ ] 2.3 Implement host auth inventory synchronization with default policies derived from Codex candidates and host priorities.
- [ ] 2.4 Add policy normalization and persistence tests covering invalid weights, reserves, profiles, corrupt files, and failed atomic writes.

## 3. Strict Scheduler

- [ ] 3.1 Add failing scheduler tests for Codex-only handling, eligibility filters, regular-before-backup behavior, profile tiers, and strict priority.
- [ ] 3.2 Implement deterministic candidate classification and strict selection-group construction.
- [ ] 3.3 Add failing tests for smooth weighted round-robin distribution and implement concurrency-safe weighted counters.
- [ ] 3.4 Add failing tests for session affinity constrained by policy revision and implement affinity TTL, rebinding, and cleanup.
- [ ] 3.5 Add retry and no-eligible-account tests and return a handled scheduling error when quota policy blocks every Codex candidate.

## 4. Quota Refresh

- [ ] 4.1 Add parser tests for primary and secondary windows, used percentage, reset fields, missing windows, and plan normalization.
- [ ] 4.2 Implement credential extraction from host auth JSON and sanitized Codex quota requests through `host.http.do`.
- [ ] 4.3 Implement quota snapshot updates that preserve the last valid values on refresh failure and never persist secrets.
- [ ] 4.4 Add coordinator tests for stable staggering, concurrency limits, duplicate suppression, manual filters, and clean shutdown.
- [ ] 4.5 Implement the refresh coordinator and delayed deduplicated refresh after Codex HTTP 429 usage records.

## 5. Management API

- [ ] 5.1 Register exact authenticated Management API routes and a static `/pool` browser resource.
- [ ] 5.2 Implement account-list, batch-policy, profile, refresh, status, and non-mutating preview handlers.
- [ ] 5.3 Add handler tests for validation, atomic batch behavior, temporary profile expiry, refresh targeting, preview stability, and secret-free responses.

## 6. Account Pool Interface

- [ ] 6.1 Build the embedded account table with plan, quota, status, priority, weight, backup, enabled, and reserve columns.
- [ ] 6.2 Add filtering, row selection, Free/paid quick selection, inline pending edits, and an apply-changes workflow.
- [ ] 6.3 Add the batch action bar, profile segmented control, temporary Free-first duration, refresh controls, and scheduling preview.
- [ ] 6.4 Verify the static resource contains no account data and test desktop and mobile layout for overflow and overlapping controls.

## 7. Integration and Documentation

- [ ] 7.1 Add focused README configuration examples for `paid-first`, `free-first`, `free-only`, state directory, refresh interval, and stale policy.
- [ ] 7.2 Build the shared library through the plugin Makefile and verify no generated binary or header is committed.
- [ ] 7.3 Run `gofmt` on all Go files, plugin unit tests, `go test ./...`, and the required main server compile command.
- [ ] 7.4 Document the exact server migration, rollback, scheduler-plugin conflict, management access restriction, and matching Linux toolchain requirements.
