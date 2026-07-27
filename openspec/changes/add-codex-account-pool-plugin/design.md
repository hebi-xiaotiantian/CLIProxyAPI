## Context

CLIProxyAPI already exposes the standard dynamic-library capabilities required by this change: Scheduler, UsagePlugin, ManagementAPI, browser resources, host auth callbacks, and host HTTP callbacks. The deployed instance currently uses a simple custom Scheduler plugin, native `fill-first` routing, and session affinity, but it has no shared account-policy store or real Codex quota snapshots.

The reference implementation in `cockpit-tools` demonstrates the desired operator model: eligibility filtering, quota reserve, regular-before-backup ordering, strict priority, weighted selection, affinity, and retry fallback. This change adopts those semantics while preserving CLIProxyAPI's plugin boundary and avoiding changes to `internal/translator/`.

Standard browser resources are not management-authenticated by the host. The resource must therefore be a static shell; all account data and state changes must use authenticated Management API routes.

## Goals / Non-Goals

**Goals:**

- Provide one editable account-pool page for all Codex accounts.
- Support strict profile tiers, account priority, smooth weighted round-robin, independent backups, affinity, and retries.
- Route against fresh five-hour and weekly subscription quota instead of token estimates.
- Refresh quota on a schedule and on operator demand without storing credential secrets.
- Keep scheduler reads fast and deterministic under concurrent requests.
- Package the feature as a standard C ABI Go plugin compatible with the existing plugin host.

**Non-Goals:**

- Owning Codex OAuth login or token refresh.
- Modifying host auth files to persist plugin policy.
- Replacing the main CLIProxyAPI management center.
- Implementing billing or cost accounting from request token counts.
- Changing provider translators or core scheduler interfaces.
- Publishing the plugin to the official plugin registry in the first iteration.

## Decisions

### 1. Keep the feature in one plugin with focused internal packages

The tracked source will live at `examples/plugin/codex-account-pool/go/`, matching existing standard plugin examples. The C ABI entry point remains a small `package main`; routing, quota, persistence, management, and affinity logic live in focused files in the same Go module.

One plugin owns Scheduler, UsagePlugin, and ManagementAPI so routing cannot observe quota or policy state from a different process at a different revision.

Alternative: separate scheduler and quota plugins. Rejected because it duplicates account identity mapping, requires cross-plugin state coordination, and permits stale routing decisions.

### 2. Use plugin-owned immutable snapshots

Policy and quota state are normalized into immutable snapshots published through atomic pointers. Scheduler calls perform no disk or network I/O. A small mutex protects smooth weighted counters and affinity bindings; persistence and quota refresh happen outside the scheduler critical path.

Policy is stored in `policy.json` and quota state in `quota.json` under configurable `state_dir`. Writes use a temporary file, file sync, permission `0600`, and atomic rename. Access tokens and raw credential or quota payloads are never persisted.

Alternative: write priority into each host auth file. Rejected because profile switching would rewrite many credentials, trigger watchers, and mix operator policy with OAuth material.

### 3. Model routing as ordered dimensions, not numeric boosts

Selection uses a tuple instead of adding large numeric offsets:

1. regular layer before backup layer;
2. active profile tier;
3. highest account priority;
4. affinity hit inside that exact group;
5. smooth weighted round-robin.

Keeping dimensions separate guarantees that a backup account cannot preempt a regular account and that a paid account cannot escape a Free-first tier through an unusually high base priority.

The host passes every currently available candidate to the plugin before applying its built-in highest-priority reduction. If the plugin leaves the request unhandled, the host restores the original highest-priority behavior before invoking the built-in selector. This lets the plugin descend to a lower plugin priority after quota filtering without changing non-plugin routing.

The built-in profiles are:

- `paid-first`: paid, Free, unknown;
- `free-first`: Free, paid, unknown;
- `free-only`: Free regular accounts, then explicitly marked backups;
- `custom`: operator-defined plan-group ordering.

### 4. Implement affinity inside the plugin

Returning a concrete `AuthID` from Scheduler bypasses the host's built-in selector wrapper. The plugin therefore derives a session key from scheduler headers and metadata, retains bindings for the configured TTL, and accepts a binding only if the account remains in the current strict selection group.

The host-provided metadata path includes explicit execution sessions and stable `derived_session_id` values produced from request context, so requests without a client session header can still retain affinity.

Profile changes publish a new policy revision. Affinity entries include that revision and are lazily invalidated, making profile changes immediately effective without a global blocking cache clear.

Weighted state is discarded when the policy revision changes. Expired or mismatched affinity entries are removed while binding new sessions, and the affinity table has a fixed maximum size with oldest-expiry eviction.

### 5. Use host callbacks for credentials and HTTP

The refresher obtains account inventory from `host.auth.list`, maps an account to `auth_index`, and reads current credential JSON with `host.auth.get` only while building a refresh request. It extracts `access_token` and `account_id` from supported top-level or nested token fields, builds the Codex headers, invokes `host.http.do`, and discards credential bytes after the call.

The endpoint defaults to `https://chatgpt.com/backend-api/wham/usage` and remains configurable for tests. The parser stores normalized fields only. Missing `plan_type` becomes `unknown`; it is never inferred as Free.

Quota windows are presence-aware. At least one valid window is required, every present window must contain a percentage in the zero-through-one-hundred range, and absent windows are not invented as fully available. A primary window with a weekly duration is normalized into the weekly slot so Free accounts with one weekly-only window remain usable and display correctly.

Alternative: direct `net/http` from the plugin. Rejected because host HTTP callbacks preserve the server's proxy and request-observability policy.

### 6. Schedule refresh with one coordinator

A background coordinator starts after successful plugin configuration and stops during plugin shutdown. It uses:

- default interval: 10 minutes;
- default maximum snapshot age: 20 minutes;
- default concurrency: 3;
- stable account hash for startup and periodic staggering;
- one in-flight refresh per account;
- delayed deduplicated refresh after Codex HTTP 429 usage records.

The scheduled inventory callback re-reads host auth inventory before each scheduling pass. Configuration reload stops the old coordinator before switching stores or snapshots, preventing an old refresh from writing into the new state directory. `stale_policy=exclude` is the default; `allow` is an explicit availability-first override.

The coordinator rejects new work once stopping begins and keeps an account marked in flight until its refresh result has been applied. Configuration reload also holds the policy and quota mutation locks while loading and publishing the replacement snapshots, preventing concurrent state updates from being overwritten by an older disk read.

### 7. Serve a static management shell and authenticated JSON routes

The resource `/v0/resource/plugins/codex-account-pool/pool` serves embedded HTML, CSS, and JavaScript only. It does not render account state into the document. The page keeps the management key in memory or session storage and calls authenticated routes:

- `GET /v0/management/codex-account-pool/status`
- `GET /v0/management/codex-account-pool/accounts`
- `PATCH /v0/management/codex-account-pool/accounts`
- `GET /v0/management/codex-account-pool/profile`
- `PUT /v0/management/codex-account-pool/profile`
- `POST /v0/management/codex-account-pool/refresh`
- `POST /v0/management/codex-account-pool/preview`

Plugin routes are exact paths because the host intentionally rejects path parameters and wildcards. Batch targets are supplied in JSON request bodies.

The UI uses a dense table, inline numeric controls, checkboxes, filters, selection, a batch action bar, profile segmented control, custom plan-order selection, refresh actions, and a preview panel. It polls queued and refreshing accounts until they reach a terminal state or a bounded attempt limit. It uses no frontend framework or build-time JavaScript dependency.

### 8. Build and migration follow server compatibility

The plugin is built with `go build -buildmode=c-shared` on Linux for the deployed architecture. The source module pins the same CLIProxyAPI module revision as the server build. The existing `low-quota-scheduler` is disabled before enabling this plugin because only the highest-priority active Scheduler is consulted.

The first deployment runs with `stale_policy=allow` until initial snapshots are populated, then switches to `exclude`. Management access is restricted to localhost or Tailscale before the account page is enabled.

## Risks / Trade-offs

- The Codex usage endpoint is not a stable public API. -> Keep endpoint and headers isolated in `quota_client.go`, preserve the last valid snapshot, and expose sanitized failures.
- A C ABI plugin runs as trusted in-process code. -> Minimize dependencies, validate every management payload, recover at the ABI boundary, and never log secrets.
- Smooth weighted round-robin requires mutable counters. -> Lock only the selected group state and keep network and persistence work outside the lock.
- Resource HTML is publicly fetchable when the server is public. -> Embed no account data, require Management API authentication for all data, and restrict management networking.
- `stale_policy=exclude` can stop routing during a prolonged quota outage. -> Expose an explicit `allow` override and clear diagnostics; do not silently bypass policy.
- Host auth files can change while a refresh runs. -> Resolve the current auth index and credential immediately before each request and never cache tokens.
- A local plugin build can be incompatible with the deployed binary. -> Build on matching Linux toolchain and deploy the server and plugin from the same tagged fork revision.
- A native plugin background `host.http.do` callback has no cancellation identifier in the current C ABI. -> Keep refresh concurrency bounded, stop accepting new work before reload, and document that shutdown can wait for an already-established non-cooperative host HTTP call.

## Migration Plan

1. Build and test the plugin from the fork on the same CLIProxyAPI revision intended for the server.
2. Restrict remote Management API access, move the management secret to a protected environment file, and normalize auth file permissions.
3. Back up the server configuration, plugin directory, and plugin state directory.
4. Install the new shared library and configure an absolute `state_dir`.
5. Disable `low-quota-scheduler`; keep the usage tracker temporarily if desired.
6. Enable `codex-account-pool` with `stale_policy=allow` and verify account inventory and manual quota refresh.
7. Configure account policies and profiles in the management table.
8. Switch to `stale_policy=exclude`, run scheduling previews, and verify live failover.
9. Roll back by disabling the new plugin, re-enabling the old scheduler, and restoring the previous configuration; plugin state files can remain unused.

## Open Questions

- Confirm the deployed server binary commit and Go version before producing the Linux artifact.
- Confirm whether the server architecture is `amd64` or `arm64`.
- Confirm the preferred absolute plugin state directory during deployment.
