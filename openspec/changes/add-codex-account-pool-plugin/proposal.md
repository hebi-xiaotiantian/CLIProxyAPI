## Why

CLIProxyAPI can prioritize credentials and delegate scheduling, but operators cannot manage a Codex account pool from one place or route against the real five-hour and weekly subscription limits. The current server deployment therefore requires manual per-account edits and cannot temporarily prefer Free accounts without rewriting credential metadata.

## What Changes

- Add a standard dynamic-library plugin named `codex-account-pool` that owns Codex credential selection.
- Add strict regular-versus-backup account layers, profile tiers, per-account priority, and smooth weighted round-robin within the highest eligible priority.
- Add plugin-owned session affinity that remains constrained by the active strict priority tier.
- Fetch and persist Codex five-hour and weekly quota snapshots, plan type, reset times, freshness, and refresh errors.
- Add staggered scheduled quota refresh, bounded concurrency, manual refresh, and delayed refresh after quota-related failures.
- Add a management resource with a single editable account table for inline priority, weight, backup, enabled, and quota-reserve controls.
- Add filters, selection, batch editing, route-profile switching, temporary Free-first overrides, and scheduling previews.
- Keep OAuth credentials in host-managed auth files; store only account policy and quota snapshots in plugin-owned state.
- Document Linux plugin builds, migration from the existing scheduler plugin, and restricted management access.

## Capabilities

### New Capabilities

- `codex-account-pool-routing`: Strict account eligibility, regular and backup layers, route profiles, priority, weighted selection, affinity, and retry behavior.
- `codex-account-quota-refresh`: Codex plan detection, five-hour and weekly quota snapshots, refresh scheduling, freshness rules, and failure handling.
- `codex-account-pool-management`: Management APIs and an editable browser resource for account policies, batch actions, route profiles, refresh controls, and scheduling previews.

### Modified Capabilities

None.

## Impact

- Adds a new Go plugin under `examples/plugin/codex-account-pool/` built through the standard C ABI.
- Uses existing Scheduler, UsagePlugin, ManagementAPI, resource route, and host auth callback interfaces without changing core scheduler contracts.
- Adds plugin-local policy and quota state files under the configured plugin data directory.
- Adds focused plugin tests and build targets.
- Deployment must disable the existing `low-quota-scheduler` Scheduler capability to avoid competing routing decisions.
- Linux plugin artifacts must be built against the same CLIProxyAPI SDK revision and compatible Go toolchain as the deployed server.
