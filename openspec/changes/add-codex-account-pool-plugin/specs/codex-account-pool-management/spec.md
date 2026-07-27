## ADDED Requirements

### Requirement: Editable account pool table
The plugin SHALL expose a browser resource that displays Codex accounts in one table with plan, quota, host status, plugin status, priority, weight, backup, enabled, and reserve controls.

#### Scenario: Account table loads
- **WHEN** an authorized operator opens the account pool resource and supplies a valid management key
- **THEN** the page loads current account and quota state from authenticated Management API routes

#### Scenario: Inline edit
- **WHEN** the operator changes priority, weight, backup, enabled, or reserve values in a table row
- **THEN** the page keeps the edit pending until the operator applies the change

#### Scenario: Apply edits
- **WHEN** the operator applies valid pending edits
- **THEN** the plugin validates and atomically persists the policy update before replacing the active routing snapshot

### Requirement: Filtering and batch editing
The management resource SHALL allow operators to filter and select accounts and apply one validated change to all selected accounts.

#### Scenario: Select all Free accounts
- **WHEN** the operator filters detected plan `free` and selects all visible rows
- **THEN** subsequent batch actions target every matching visible account

#### Scenario: Batch priority and weight
- **WHEN** the operator sets priority and weight for selected accounts
- **THEN** the plugin applies both values atomically to all selected policies

#### Scenario: Invalid batch update
- **WHEN** any submitted batch value fails validation
- **THEN** the plugin rejects the whole update and leaves all policies unchanged

### Requirement: Profile controls
The management resource SHALL expose persistent and temporary route-profile controls without modifying per-account base priorities or weights.

#### Scenario: Automatic profile selection
- **WHEN** the operator activates an automatic quota or subscription profile
- **THEN** the page displays its ordering rule and visually disables manual priority and weight controls while retaining their saved values

#### Scenario: Persistent Free-first selection
- **WHEN** the operator activates `free-first` without an expiration
- **THEN** the plugin persists it as the active profile and applies it to subsequent scheduling decisions

#### Scenario: Temporary Free-first selection
- **WHEN** the operator activates `free-first` with a duration
- **THEN** the plugin applies the override immediately and displays its expiration time

#### Scenario: Temporary override is cleared
- **WHEN** the operator clears the override
- **THEN** the persistent profile becomes effective immediately

#### Scenario: Custom plan order
- **WHEN** the operator activates `custom` and chooses a plan order
- **THEN** the plugin validates and persists that order for subsequent scheduling decisions

### Requirement: Quota controls and diagnostics
The management resource SHALL provide manual quota refresh, next-refresh visibility, snapshot freshness, sanitized errors, and per-account refresh status.

#### Scenario: Refresh selected accounts
- **WHEN** the operator selects accounts and invokes refresh
- **THEN** the page polls and shows queued, refreshing, succeeded, or failed state for each selected account until work reaches a terminal state or the bounded polling limit is reached

#### Scenario: Refresh error is displayed
- **WHEN** an account refresh fails
- **THEN** the page displays a sanitized error without exposing tokens or raw credential JSON

### Requirement: Scheduling preview
The management API SHALL return an ordered, non-mutating scheduling preview for a supplied model and optional session identifier, and the browser resource SHALL render it as an operator-facing route explanation rather than raw JSON.

#### Scenario: Preview current order
- **WHEN** an operator requests a preview
- **THEN** the response lists funnel counts, grouped filtered reasons, regular and backup layers, the active strict or automatic group, and the predicted first choice

#### Scenario: Preview is rendered
- **WHEN** a preview response succeeds
- **THEN** the page displays a route funnel, a selected-account summary, an ordered candidate table, and human-readable exclusion reasons without exposing raw JSON

#### Scenario: Preview does not consume weight
- **WHEN** a preview is requested repeatedly
- **THEN** smooth weighted round-robin counters and session bindings remain unchanged

### Requirement: Recent real scheduling decisions
The plugin SHALL retain a bounded in-memory history of real Scheduler selections and SHALL expose it through an authenticated Management API route.

#### Scenario: Real request is scheduled
- **WHEN** the host invokes the plugin Scheduler and the plugin selects an account
- **THEN** the plugin records timestamp, model, selected account, active profile, active layer, candidate count, eligible count, and selection-group size

#### Scenario: Preview is requested
- **WHEN** an operator runs a scheduling preview
- **THEN** the plugin does not add a recent-decision entry

#### Scenario: Decision history is bounded and secret-free
- **WHEN** more than the configured maximum number of decisions have occurred
- **THEN** the oldest entries are discarded and retained entries contain no request body, credential, token, or raw session identifier

#### Scenario: Recent decisions are displayed
- **WHEN** the operator opens the recent scheduling view
- **THEN** the page displays the newest real decisions in a readable table and distinguishes them from non-mutating previews

### Requirement: Management API security
Account data and state-changing operations SHALL be available only through authenticated Management API routes, while the browser resource SHALL contain static assets only.

#### Scenario: Resource is fetched without management authentication
- **WHEN** a client requests the plugin browser resource
- **THEN** the returned HTML contains no account identifiers, emails, quota data, policy state, or credential data

#### Scenario: API request lacks a valid management key
- **WHEN** a client calls an account-pool Management API route without valid host management authentication
- **THEN** the host rejects the request before plugin state is read or changed

### Requirement: Configuration recovery
The plugin SHALL retain the previous valid policy state when persisted policy loading or a management update fails.

#### Scenario: Corrupt policy file
- **WHEN** the plugin starts with an unreadable or invalid policy file
- **THEN** it reports degraded state, keeps a safe in-memory default, and does not overwrite the corrupt file automatically

#### Scenario: Atomic write fails
- **WHEN** persisting a policy update fails
- **THEN** the plugin returns an error and continues using the prior in-memory policy snapshot

#### Scenario: Configuration reload overlaps a state update
- **WHEN** configuration reload overlaps a policy mutation or quota result commit
- **THEN** state loading and snapshot replacement are serialized with those mutations so a stale file read cannot overwrite the newer state
