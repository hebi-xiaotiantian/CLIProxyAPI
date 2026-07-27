# Codex Account Token Usage Design

## Context

The existing `codex-account-pool` plugin owns Codex account scheduling, quota
refresh, and its management page. It already registers the host `UsagePlugin`
capability, but currently uses usage records only to react to HTTP 429 failures.

CLIProxyAPI already extracts Codex token usage from upstream responses and
delivers it to dynamic usage plugins with the selected credential `AuthID`.
Therefore, account-level token aggregation can remain entirely inside the
existing plugin. It does not require another plugin or changes to executors,
translators, the scheduler contract, or the core Management API.

This feature is distinct from ChatGPT subscription quota. The existing
five-hour and weekly percentages come from the Codex quota endpoint. The new
token counters cover only requests that pass through this CLIProxyAPI instance.

## Goals

- Persist cumulative Codex token usage per account across process restarts.
- Display the cumulative totals directly in the existing account-pool table.
- Keep usage ingestion fast and independent from routing decisions.
- Preserve the existing HTTP 429 refresh behavior.
- Avoid storing credentials, request bodies, response bodies, or API keys.
- Follow the plugin's existing atomic JSON persistence model.

## Non-Goals

- Estimating remaining ChatGPT quota from local token totals.
- Treating local totals as the account's global OpenAI usage.
- Grouping usage by day, model, API key, or arbitrary time range.
- Providing request-level audit logs or cost estimation.
- Using token totals to influence account eligibility, priority, weight, or
  route selection.
- Adding a usage reset workflow in the first iteration.

## Considered Approaches

### 1. In-memory aggregation with delayed atomic JSON persistence

The usage callback updates an in-memory snapshot and signals a background
writer. The writer coalesces updates and persists the newest snapshot at most
once per second. Plugin reconfiguration and shutdown perform a final
synchronous flush.

This is the selected approach. It keeps the callback fast, matches the existing
`policy.json` and `quota.json` state model, and satisfies restart persistence
without introducing a database.

### 2. Synchronous JSON persistence for every usage record

This has less coordination code but would perform encoding, file sync, and
directory sync in the usage dispatch path. It is rejected because high request
rates would unnecessarily block the usage manager.

### 3. SQLite request-event storage

This would support custom time ranges, daily summaries, model breakdowns, and
request logs. It is rejected for this iteration because the requested scope is
only a persistent cumulative account total.

## Architecture

One plugin continues to own `Scheduler`, `UsagePlugin`, and `ManagementAPI`.
The new tracker is an internal observation component:

```text
Codex upstream response usage
        |
        v
CLIProxyAPI UsageRecord(AuthID, Detail)
        |
        v
codex-account-pool UsagePlugin
        |
        +--> preserve existing HTTP 429 quota refresh behavior
        |
        +--> update account usage snapshot in memory
                  |
                  v
          delayed atomic usage.json write
                  |
                  v
GET /v0/management/codex-account-pool/accounts
                  |
                  v
existing /pool account table
```

The scheduler never reads token totals. Its current immutable policy and quota
snapshots remain unchanged.

## Data Model

The plugin adds `usage.json` under the configured `state_dir`:

```text
state_dir/
  policy.json
  quota.json
  usage.json
```

The document uses the host credential `AuthID` as its stable account key:

```json
{
  "version": 1,
  "accounts": {
    "auth-id": {
      "requests": 120,
      "failed_requests": 3,
      "input_tokens": 8200000,
      "output_tokens": 2310000,
      "reasoning_tokens": 1620000,
      "cache_read_tokens": 350000,
      "cache_creation_tokens": 0,
      "total_tokens": 10510000,
      "updated_at": "2026-07-27T10:00:00Z"
    }
  }
}
```

All counters use signed 64-bit integers with saturating addition. Negative
values received from a usage record are treated as zero.

The persisted document retains entries for accounts that are temporarily absent
from host inventory. Such entries are not returned in the visible account list.
If the same `AuthID` returns, its cumulative total becomes visible again.

## Accounting Rules

The tracker accepts only records whose provider is `codex` and whose `AuthID`
is non-empty.

Each upstream usage record is counted independently. This means a retry that
actually consumed upstream tokens is included in the total. A failed attempt
with reported usage also contributes tokens. A failure without usage increments
only `requests` and `failed_requests`.

The request counters therefore mean observed provider usage records, not a
guaranteed count of every client request. A successful upstream response that
contains no usage payload may not produce a record for this plugin.

The tracker preserves these reported fields:

- input tokens;
- output tokens;
- reasoning tokens;
- cache-read tokens;
- cache-creation tokens;
- total tokens.

For Codex, reasoning tokens are a subset of output tokens, and cache tokens are
a subset of input tokens. They are displayed as detail but are not added again
when calculating the total.

The legacy `CachedTokens` field aliases cache reads for Codex. Ingestion
normalizes cache reads as the larger of `CacheReadTokens` and `CachedTokens`,
then stores cache creation separately. The management view reports
`cache_tokens` as normalized cache reads plus cache creation, using saturating
addition.

`Detail.TotalTokens` is authoritative when it is positive. If it is absent or
zero while input or output tokens are present, the tracker falls back to
`input_tokens + output_tokens`. Reasoning and cache counters are not included
again in that fallback.

## Runtime Lifecycle

A focused `usageTracker` owns:

- the mutable account totals;
- a dirty state;
- a wake-up channel for persistence;
- the delayed writer goroutine;
- final flush and shutdown coordination.

Normal ingestion performs only validation, bounded integer addition, an
in-memory update, and a non-blocking writer notification.

The writer waits up to one second to coalesce bursts, clones the current
document, and writes it through the existing atomic state-store primitive:

1. create a temporary file in `state_dir`;
2. set mode `0600`;
3. encode the normalized JSON document;
4. sync and close the temporary file;
5. rename it over `usage.json`;
6. enforce mode `0600`;
7. sync the state directory.

On a successful save, the tracker clears the dirty state only if no newer
mutation occurred while the snapshot was being written. On failure, the latest
in-memory state remains dirty and the writer retries once per second while it
remains dirty.

The tracker remains active across ordinary plugin configuration reloads. If
`state_dir` is unchanged, the in-memory document and writer continue without a
reload. If `state_dir` changes, ingestion is briefly blocked while the tracker
flushes the old store and loads the new document. A failed final flush aborts
the directory switch and retains the old store and in-memory totals. This
prevents events from being written to the wrong state directory or being lost
during the switch.

Shutdown blocks new tracker ingestion, attempts one final synchronous flush,
and logs a sanitized error if the final persistence attempt fails.

## Management API

No new route is required. The existing authenticated endpoint remains the
single account-table data source:

```text
GET /v0/management/codex-account-pool/accounts
```

Each account view gains a nested `usage` object:

```json
{
  "id": "auth-id",
  "usage": {
    "requests": 120,
    "failed_requests": 3,
    "input_tokens": 8200000,
    "output_tokens": 2310000,
    "reasoning_tokens": 1620000,
    "cache_tokens": 350000,
    "total_tokens": 10510000,
    "updated_at": "2026-07-27T10:00:00Z"
  }
}
```

`cache_tokens` is a display-oriented value derived without double-counting
legacy cached-token aliases. The internal persisted form retains cache-read and
cache-creation counters separately.

Accounts without a usage snapshot omit the `usage` object. The browser renders
an absent object as unavailable.

The plugin status response also exposes sanitized usage persistence health, such
as a degraded flag and the latest persistence error. No response contains
credential material.

## Management Interface

The existing account table adds one `Token Usage` column between weekly quota
and account status.

The cell uses the approved compact expanded layout:

```text
Total 10.51M
Input 8.20M · Output 2.31M · Reasoning 1.62M · Cache 350K
```

Display rules:

- render `--` when no usage has been observed;
- format values with compact `K`, `M`, and `B` suffixes;
- expose exact integer values, request count, failure count, and last update
  time in hover details;
- label the metric as proxy cumulative usage so it is not confused with global
  OpenAI account usage;
- keep pending priority, weight, backup, enabled, and reserve edits intact when
  new usage data arrives.

The page refreshes only the account list every five seconds while the document
is visible. Polling pauses while the page is hidden. A polling failure preserves
the last rendered account data and updates only the connection status.

The static resource continues to embed no account data. All live data remains
behind the Management API key.

## Error Handling

- A missing `usage.json` starts with an empty document.
- A malformed or unsupported document starts with empty in-memory totals and
  exposes a sanitized degraded status.
- A save failure does not fail provider requests, quota refresh, or scheduling.
- Dirty in-memory data remains available to the management page and is retried.
- Usage from unknown or currently absent accounts remains persisted but hidden.
- Browser polling errors do not clear the existing table.
- Existing error sanitization remains in force, and no request or response body
  is retained by the tracker.

## Testing

Focused unit tests will cover:

- Codex-only filtering and missing `AuthID` filtering;
- per-account isolation;
- successful and failed request counters;
- failures with and without token detail;
- retries represented as multiple real upstream usage records;
- authoritative total usage and the input-plus-output fallback;
- reasoning and cache subset counters without total double-counting;
- zero and negative values;
- saturating integer addition;
- coalesced delayed persistence;
- restart loading;
- final flush on shutdown and reconfiguration;
- corrupt state and save-failure retry behavior;
- Management API serialization and secret-free responses;
- browser markup, compact number formatting, polling, and preservation of
  pending edits.

Final verification will include:

```text
gofmt -w .
(cd examples/plugin/codex-account-pool/go && go test ./...)
go test ./...
go build -o test-output ./cmd/server
rm test-output
(cd examples/plugin/codex-account-pool/go && \
  go build -buildmode=c-shared -o test-codex-account-pool.so . && \
  rm -f test-codex-account-pool.so test-codex-account-pool.h)
```

Generated shared-library outputs and C headers will not be committed.
