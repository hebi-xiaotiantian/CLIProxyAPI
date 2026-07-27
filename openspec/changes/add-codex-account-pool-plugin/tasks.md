## 1. Plugin Scaffold

- [x] 1.1 Create `examples/plugin/codex-account-pool/go/go.mod`, the C ABI entry point, lifecycle configuration, and capability registration for Scheduler, UsagePlugin, and ManagementAPI.
- [x] 1.2 Add the plugin to `examples/plugin/Makefile` and document its configuration and Linux build command.
- [x] 1.3 Add ABI boundary tests for registration, configuration validation, shutdown, and panic-safe error envelopes.

## 2. Policy and State

- [x] 2.1 Define validated account policy, route profile, temporary override, quota snapshot, refresh status, and persisted state types.
- [x] 2.2 Implement atomic `0600` JSON persistence that retains the previous in-memory snapshot when loading or saving fails.
- [x] 2.3 Implement host auth inventory synchronization with default policies derived from Codex candidates and host priorities.
- [x] 2.4 Add policy normalization and persistence tests covering invalid weights, reserves, profiles, corrupt files, and failed atomic writes.

## 3. Strict Scheduler

- [x] 3.1 Add failing scheduler tests for Codex-only handling, eligibility filters, regular-before-backup behavior, profile tiers, and strict priority.
- [x] 3.2 Implement deterministic candidate classification and strict selection-group construction.
- [x] 3.3 Add failing tests for smooth weighted round-robin distribution and implement concurrency-safe weighted counters.
- [x] 3.4 Add failing tests for session affinity constrained by policy revision and implement affinity TTL, rebinding, and cleanup.
- [x] 3.5 Add retry and no-eligible-account tests and return a handled scheduling error when quota policy blocks every Codex candidate.

## 4. Quota Refresh

- [x] 4.1 Add parser tests for primary and secondary windows, used percentage, reset fields, missing windows, and plan normalization.
- [x] 4.2 Implement credential extraction from host auth JSON and sanitized Codex quota requests through `host.http.do`.
- [x] 4.3 Implement quota snapshot updates that preserve the last valid values on refresh failure and never persist secrets.
- [x] 4.4 Add coordinator tests for stable staggering, concurrency limits, duplicate suppression, manual filters, and clean shutdown.
- [x] 4.5 Implement the refresh coordinator and delayed deduplicated refresh after Codex HTTP 429 usage records.

## 5. Management API

- [x] 5.1 Register exact authenticated Management API routes and a static `/pool` browser resource.
- [x] 5.2 Implement account-list, batch-policy, profile, refresh, status, and non-mutating preview handlers.
- [x] 5.3 Add handler tests for validation, atomic batch behavior, temporary profile expiry, refresh targeting, preview stability, and secret-free responses.

## 6. Account Pool Interface

- [x] 6.1 Build the embedded account table with plan, quota, status, priority, weight, backup, enabled, and reserve columns.
- [x] 6.2 Add filtering, row selection, Free/paid quick selection, inline pending edits, and an apply-changes workflow.
- [x] 6.3 Add the batch action bar, profile segmented control, temporary Free-first duration, refresh controls, and scheduling preview.
- [x] 6.4 Verify the static resource contains no account data and test desktop and mobile layout for overflow and overlapping controls.

## 7. Integration and Documentation

- [x] 7.1 Add focused README configuration examples for `paid-first`, `free-first`, `free-only`, state directory, refresh interval, and stale policy.
- [x] 7.2 Build the shared library through the plugin Makefile and verify no generated binary or header is committed.
- [x] 7.3 Run `gofmt` on all Go files, plugin unit tests, `go test ./...`, and the required main server compile command.
- [x] 7.4 Document the exact server migration, rollback, scheduler-plugin conflict, management access restriction, and matching Linux toolchain requirements.

## 8. Review Hardening

- [x] 8.1 Pass all host-available priority levels to plugin schedulers and preserve built-in highest-priority fallback when the plugin is unhandled.
- [x] 8.2 Serialize policy mutations and stop the old refresh coordinator before switching state during reconfiguration.
- [x] 8.3 Synchronize host inventory during scheduled refresh and update automatic reserve defaults after plan detection without overwriting operator values.
- [x] 8.4 Validate present quota windows, support weekly-only primary windows, and expose window presence to routing and management.
- [x] 8.5 Bound smooth weighted and affinity state, release quota locks on invalid refresh results, sync state directories, and disable JSON response caching.
- [x] 8.6 Add custom plan-order controls and bounded polling for queued or refreshing accounts in the browser resource.
- [x] 8.7 Cover derived-session affinity, serialized configuration state loading, result-commit duplicate suppression, and stop-safe coordinator lifecycle with regression tests.

## 9. Automatic Routing Modes

- [x] 9.1 Add failing scheduler tests for `auto`, quota high/low, plan high/low, unknown metrics, reserve-before-ordering, exact ties, backup fallback, and automatic affinity invalidation.
- [x] 9.2 Implement plan-rank normalization and presence-aware minimum remaining-quota scoring.
- [x] 9.3 Implement automatic strict-group construction while preserving the existing paid-first, free-first, free-only, and custom behavior.
- [x] 9.4 Make automatic modes ignore manual priority and weight, rotate exact ties equally, and keep persisted custom values unchanged.
- [x] 9.5 Extend policy validation, profile management, README examples, and migration compatibility tests for the new profile values.

## 10. Visual Diagnostics

- [x] 10.1 Add failing management tests for funnel counts, human-readable candidate fields, grouped exclusion reasons, and non-mutating previews.
- [x] 10.2 Add a bounded, secret-free real-decision ring and authenticated `GET /codex-account-pool/decisions` route with tests proving previews do not create entries.
- [x] 10.3 Replace raw JSON preview rendering with a route funnel, selected-account summary, ordered candidate table, and grouped exclusion reasons.
- [x] 10.4 Add a recent-real-scheduling table and grouped route-mode control; visually disable priority and weight inputs in automatic modes without deleting values.
- [ ] 10.5 Verify responsive layout, static-resource secrecy, plugin tests, race tests, vet, host tests, server compile, Linux shared-library build, OpenSpec validation, Home mode disabled, and one real request-to-decision smoke test.
