# Codex Account Pool Plugin

This standard C ABI plugin provides one quota-aware Codex account pool for CLIProxyAPI.

## Routing

Every scheduler mode first applies:

1. Host and plugin eligibility.
2. Fresh five-hour and weekly quota reserves.
3. Regular accounts before backup accounts.

Automatic profiles ignore saved account priority and weight:

- `auto`: highest known subscription rank, then highest remaining quota.
- `quota-high-first`: highest remaining quota, then highest known subscription rank.
- `quota-low-first`: lowest remaining quota that still satisfies reserves, then highest known subscription rank.
- `plan-high-first`: highest known subscription rank, then highest remaining quota.
- `plan-low-first`: lowest known subscription rank, then highest remaining quota.

Remaining quota is the minimum percentage across every present quota window. Subscription order is Free, Go, Plus/Team/Business, Pro/ProLite, ProMax, then Enterprise-family plans. Unknown metrics always rank after known metrics. Accounts tied on the complete automatic key rotate equally.

Strict profiles preserve manual controls:

- `paid-first`: paid, Free, unknown, then backups.
- `free-first`: Free, paid, unknown, then backups.
- `free-only`: Free regular accounts, then explicitly marked backups.
- `custom`: plan order from the persisted policy document.

Inside the selected strict tier, the scheduler chooses the highest numeric priority and uses smooth weighted round-robin among accounts at that exact priority. Switching to an automatic profile does not delete priority or weight values; they become active again when a strict profile is selected.

Session affinity is valid only while the bound account remains in the current exact selection group. Quota refreshes and profile changes can therefore move a session when another account becomes the unique winner.

CLIProxyAPI Home control-plane mode bypasses local plugin schedulers. If the server is started with a non-empty `-home-jwt`, real requests will not use this account pool and will not create decision-history entries. Leave `-home-jwt` unset for this plugin to own routing.

## Configuration

```yaml
plugins:
  enabled: true
  dir: /opt/cli-proxy/plugins
  configs:
    codex-account-pool:
      enabled: true
      priority: 100
      state_dir: /opt/cli-proxy/state/codex-account-pool
      refresh_interval: 10m
      refresh_concurrency: 3
      snapshot_max_age: 20m
      stale_policy: exclude
      affinity_ttl: 30m
      custom_plan_order: [paid, free, unknown]
      free_reserve_default: 0
      paid_reserve_default: 10
      unknown_reserve_default: 10
```

Use an absolute `state_dir` in production. `policy.json`, `quota.json`, and
`usage.json` are written with mode `0600`. They never contain OAuth tokens, raw
credential JSON, request bodies, or response bodies.

Quota windows are presence-aware. A weekly-only primary window, which can occur on Free accounts, is shown and evaluated as weekly quota; an absent five-hour or weekly window is displayed as `--` and does not receive an invented percentage.

## Token Usage

The account table displays cumulative token usage observed by this CLIProxyAPI
instance for each Codex `AuthID`. It shows input, output, reasoning, total token,
request, failed-request, and combined cache counters. `usage.json` stores
cache-read and cache-creation counters separately. Reasoning tokens remain a
subset of output tokens, and cache tokens remain a subset of input tokens, so
neither is added again to the total.

Usage is aggregated in memory. Routine background writes are coalesced and save
`usage.json` atomically at most once per second. Plugin shutdown and
`state_dir` switches may immediately flush an additional dirty snapshot. These
counters are local observations, not the account's global OpenAI or ChatGPT
subscription usage, and they never affect priority, weight, eligibility,
reserves, or route selection.

While the browser is visible and idle, it polls `/accounts` every five seconds.
During a manual quota refresh, this is replaced by status polling every two
seconds for up to 59 follow-up polls. Polling pauses while the page is hidden,
and account updates preserve unsaved policy edits.

The browser resource is exposed at:

```text
/v0/resource/plugins/codex-account-pool/pool
```

The static resource prompts for the host Management API key. All account data and state changes use authenticated `/v0/management/codex-account-pool/*` routes.

The page provides:

- grouped automatic and strict route controls;
- batch policy editing for selected accounts;
- a visual route funnel with the predicted account, ordered candidates, and grouped exclusion reasons;
- the latest 100 real scheduler decisions, kept only in memory and shown newest first.

Preview requests do not consume round-robin state, create affinity bindings, or add real-decision entries. Decision history stores only timestamp, model, selected auth ID, profile, layer, and aggregate candidate counts. It does not store request bodies, credentials, tokens, headers, or raw session identifiers.

## Build

The plugin must be built for the target server operating system and architecture from the same CLIProxyAPI revision:

```bash
cd examples/plugin/codex-account-pool/go
go test ./...
go build -buildmode=c-shared -o codex-account-pool.so .
```

On Linux, install the generated `.so` in the configured plugin directory. Do not copy a macOS `.dylib` to the server.

## Migration

1. Confirm the deployed CLIProxyAPI commit, Go version, operating system, and architecture.
2. Back up the server configuration, current plugins, and auth directory.
3. Restrict Management API access to localhost or Tailscale and protect its secret with a `0600` environment file.
4. Build the server and plugin from the same fork revision on Linux.
5. Install the plugin and configure an absolute `state_dir`.
6. Confirm the server is not started with `-home-jwt`.
7. Disable `low-quota-scheduler`; only one Scheduler plugin should own routing.
8. Start with `stale_policy: allow`, open the account pool page, and refresh all quotas.
9. Configure priorities, weights, backups, and reserves, then switch to `stale_policy: exclude`.

Rollback by disabling `codex-account-pool`, re-enabling the prior scheduler, and restoring the previous configuration. Plugin state files can remain on disk.
