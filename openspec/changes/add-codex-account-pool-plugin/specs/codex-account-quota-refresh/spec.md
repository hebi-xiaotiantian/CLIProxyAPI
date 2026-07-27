## ADDED Requirements

### Requirement: Real Codex quota snapshots
The plugin SHALL query the Codex usage endpoint and store normalized five-hour and weekly remaining percentages, reset times, window durations, plan type, refresh time, and refresh error state per account.

#### Scenario: Primary and secondary windows returned
- **WHEN** the usage response contains primary and secondary rate-limit windows
- **THEN** the plugin maps the primary window to five-hour quota and the secondary window to weekly quota

#### Scenario: Used percentage returned
- **WHEN** a window reports `used_percent`
- **THEN** the plugin stores remaining percentage as `100 - used_percent`, clamped to the range zero through one hundred

#### Scenario: Plan type returned
- **WHEN** the usage response contains a non-empty plan type
- **THEN** the plugin normalizes and stores the detected plan without changing the host auth file

#### Scenario: Weekly-only primary window returned
- **WHEN** the usage response contains one primary window whose duration is weekly and no secondary window
- **THEN** the plugin records a present weekly window, records the five-hour window as absent, and keeps the snapshot usable

#### Scenario: Invalid present window
- **WHEN** a present quota window omits `used_percent` or reports a value outside zero through one hundred
- **THEN** the refresh fails and the plugin preserves the previous valid snapshot

#### Scenario: No quota windows returned
- **WHEN** the usage response contains neither a primary nor a secondary window
- **THEN** the refresh fails instead of inventing fully available quota

### Requirement: Credential-safe quota requests
The plugin SHALL read OAuth credential JSON through the host auth callback only for the duration of a refresh request and SHALL never persist or log access tokens, refresh tokens, ID tokens, or raw credential JSON.

#### Scenario: Refresh uses host credential
- **WHEN** a Codex account refresh starts
- **THEN** the plugin resolves the account auth index, reads the current credential JSON, and sends the request through `host.http.do`

#### Scenario: Account identifier is available
- **WHEN** credential JSON contains an account identifier
- **THEN** the plugin includes it in the Codex quota request header

#### Scenario: State is persisted
- **WHEN** policy and quota state files are written
- **THEN** neither file contains credential secrets or the raw quota response body

### Requirement: Scheduled refresh
The plugin SHALL refresh eligible Codex OAuth accounts on a configurable interval with deterministic startup staggering, bounded concurrency, and per-account duplicate suppression.

#### Scenario: Plugin starts
- **WHEN** the plugin is configured with automatic refresh enabled
- **THEN** it schedules every eligible Codex account using a stable per-account offset instead of refreshing all accounts simultaneously

#### Scenario: Host inventory changes
- **WHEN** Codex auth files are added or removed while the plugin is running
- **THEN** a later scheduled refresh pass synchronizes the latest inventory without requiring the management page to be opened

#### Scenario: Refresh concurrency limit
- **WHEN** more accounts are due than the configured concurrency
- **THEN** no more than the configured number of quota requests execute concurrently

#### Scenario: Duplicate refresh request
- **WHEN** an account is already being refreshed and another automatic or manual refresh targets it
- **THEN** the plugin reuses or skips the in-flight work rather than issuing a duplicate upstream request

#### Scenario: Refresh result is being committed
- **WHEN** an upstream refresh has completed but its normalized result is still being applied or persisted
- **THEN** the account remains in flight and a duplicate refresh is not accepted

#### Scenario: Coordinator stops
- **WHEN** plugin shutdown or reconfiguration stops the refresh coordinator
- **THEN** the coordinator rejects new scheduled, delayed, or manual refresh work before waiting for existing work

### Requirement: Manual and failure-triggered refresh
The plugin SHALL support immediate refresh for all accounts, filtered account groups, and explicit account IDs, and SHALL schedule a delayed refresh after quota-related request failures.

#### Scenario: Manual Free-account refresh
- **WHEN** an authenticated management request targets detected plan `free`
- **THEN** the plugin queues refresh for all matching Codex accounts

#### Scenario: Quota-related 429
- **WHEN** UsagePlugin receives a failed Codex usage record with HTTP status 429
- **THEN** the plugin records the failure and queues a deduplicated delayed quota refresh for that account

### Requirement: Snapshot freshness policy
The plugin SHALL mark snapshots stale after the configured maximum age and SHALL expose a configurable `exclude` or `allow` stale-snapshot policy.

#### Scenario: Stale policy is exclude
- **WHEN** an account snapshot is missing or older than the maximum age
- **THEN** quota-aware routing excludes the account

#### Scenario: Stale policy is allow
- **WHEN** an account snapshot is missing or stale
- **THEN** routing may use the account while exposing the stale state in management diagnostics

### Requirement: Durable atomic state
The plugin SHALL persist quota snapshots atomically and SHALL retain the last valid snapshot when a refresh fails.

#### Scenario: Refresh succeeds
- **WHEN** a valid usage response is received
- **THEN** the plugin atomically replaces the account snapshot and clears its refresh error

#### Scenario: Refresh fails
- **WHEN** credential loading, transport, status validation, or response parsing fails
- **THEN** the plugin preserves the previous valid quota values and records a sanitized error and failure time

#### Scenario: Plugin restarts
- **WHEN** the plugin loads a valid persisted quota state
- **THEN** it makes the snapshots available immediately and evaluates freshness from their recorded timestamps
