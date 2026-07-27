# Codex Account Pool Plugin

This standard C ABI plugin provides one quota-aware Codex account pool for CLIProxyAPI.

## Routing

The scheduler applies these dimensions in order:

1. Host and plugin eligibility.
2. Fresh five-hour and weekly quota reserves.
3. Regular accounts before backup accounts.
4. Active profile tier.
5. Highest account priority.
6. Session affinity inside the active strict group.
7. Smooth weighted round-robin.

Built-in profiles:

- `paid-first`: paid, Free, unknown, then backups.
- `free-first`: Free, paid, unknown, then backups.
- `free-only`: Free regular accounts, then explicitly marked backups.
- `custom`: plan order from the persisted policy document.

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

Use an absolute `state_dir` in production. `policy.json` and `quota.json` are written with mode `0600`. They never contain OAuth tokens or raw credential JSON.

Quota windows are presence-aware. A weekly-only primary window, which can occur on Free accounts, is shown and evaluated as weekly quota; an absent five-hour or weekly window is displayed as `--` and does not receive an invented percentage.

The browser resource is exposed at:

```text
/v0/resource/plugins/codex-account-pool/pool
```

The static resource prompts for the host Management API key. All account data and state changes use authenticated `/v0/management/codex-account-pool/*` routes.

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
6. Disable `low-quota-scheduler`; only one Scheduler plugin should own routing.
7. Start with `stale_policy: allow`, open the account pool page, and refresh all quotas.
8. Configure priorities, weights, backups, and reserves, then switch to `stale_policy: exclude`.

Rollback by disabling `codex-account-pool`, re-enabling the prior scheduler, and restoring the previous configuration. Plugin state files can remain on disk.
