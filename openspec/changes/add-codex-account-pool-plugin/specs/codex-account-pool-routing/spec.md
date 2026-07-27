## ADDED Requirements

### Requirement: Codex-only scheduler ownership
The plugin SHALL handle scheduler requests only when at least one candidate belongs to the Codex provider and SHALL leave unrelated provider requests unhandled.

#### Scenario: Non-Codex scheduling request
- **WHEN** the host invokes the plugin with candidates that do not belong to the Codex provider
- **THEN** the plugin returns an unhandled scheduler response without changing candidate state

### Requirement: Policy-based eligibility
The plugin SHALL exclude accounts that are plugin-disabled, host-disabled, host-unavailable, outside the active profile, below a configured quota reserve, or associated with an unusable quota snapshot.

#### Scenario: Disabled account
- **WHEN** an account policy has `enabled` set to false
- **THEN** the scheduler does not select that account

#### Scenario: Quota reserve reached
- **WHEN** either fresh five-hour remaining quota or fresh weekly remaining quota is below the account policy reserve
- **THEN** the scheduler excludes the account until a later snapshot satisfies both reserves

#### Scenario: Unknown account policy
- **WHEN** a host candidate has no stored account policy
- **THEN** the plugin treats it as enabled, regular, weight one, and uses the host candidate priority as its base priority

### Requirement: Independent backup layer
The plugin SHALL partition eligible accounts into regular and backup layers and SHALL select from the backup layer only when no regular account remains eligible for the current request.

#### Scenario: High-priority backup does not preempt regular account
- **WHEN** a backup account has a higher numeric priority than an eligible regular account
- **THEN** the scheduler selects from the regular layer

#### Scenario: Regular layer exhausted
- **WHEN** every regular account is filtered or already tried
- **THEN** the scheduler evaluates eligible backup accounts using the same profile, priority, and weight rules

### Requirement: Route profiles
The plugin SHALL support `auto`, `quota-high-first`, `quota-low-first`, `plan-high-first`, `plan-low-first`, `paid-first`, `free-first`, `free-only`, and `custom` profiles without rewriting host auth files.

#### Scenario: Automatic profile
- **WHEN** `auto` is active
- **THEN** the scheduler selects the highest known subscription rank, then the highest known remaining quota, and rotates equally among exact ties

#### Scenario: Low-quota profile
- **WHEN** `quota-low-first` is active
- **THEN** the scheduler selects the lowest known remaining quota that still satisfies every configured reserve, then the highest known subscription rank

#### Scenario: High-quota profile
- **WHEN** `quota-high-first` is active
- **THEN** the scheduler selects the highest known remaining quota, then the highest known subscription rank

#### Scenario: Low-subscription profile
- **WHEN** `plan-low-first` is active
- **THEN** the scheduler selects the lowest known subscription rank, then the highest known remaining quota

#### Scenario: High-subscription profile
- **WHEN** `plan-high-first` is active
- **THEN** the scheduler selects the highest known subscription rank, then the highest known remaining quota

#### Scenario: Unknown automatic metric
- **WHEN** an otherwise eligible account has an unknown subscription rank or remaining-quota score
- **THEN** the scheduler ranks it after accounts having known values for the active automatic profile

#### Scenario: Free-first profile
- **WHEN** `free-first` is active and eligible Free and paid regular accounts exist
- **THEN** the scheduler evaluates the Free profile tier before the paid profile tier

#### Scenario: Paid-first profile
- **WHEN** `paid-first` is active and eligible Free and paid regular accounts exist
- **THEN** the scheduler evaluates the paid profile tier before the Free profile tier

#### Scenario: Free-only profile
- **WHEN** `free-only` is active
- **THEN** paid regular accounts are excluded while explicitly marked backup accounts remain eligible as the final layer

#### Scenario: Temporary profile expires
- **WHEN** a temporary profile override reaches its expiration time
- **THEN** the scheduler atomically returns to the configured persistent profile

### Requirement: Automatic routing metrics
The plugin SHALL derive subscription rank from normalized plan type and remaining quota from the minimum remaining percentage across every present quota window.

#### Scenario: Both quota windows are present
- **WHEN** an account has 70 percent five-hour quota and 40 percent weekly quota
- **THEN** its automatic remaining-quota score is 40

#### Scenario: Weekly-only quota is present
- **WHEN** an account has an explicitly absent five-hour window and 60 percent weekly quota
- **THEN** its automatic remaining-quota score is 60

#### Scenario: Subscription rank ordering
- **WHEN** Free, Go, Plus, Pro, ProMax, and Enterprise-family accounts are eligible
- **THEN** their subscription ranks increase in that order

#### Scenario: Reserve precedes low-quota ordering
- **WHEN** a low-quota account is below either configured reserve
- **THEN** the scheduler excludes it before comparing automatic routing scores

### Requirement: Automatic and strict modes are exclusive
The plugin SHALL ignore manual priority and weight while an automatic quota or subscription profile is active and SHALL retain those persisted values for later strict-profile use.

#### Scenario: Automatic mode ignores high manual priority
- **WHEN** `quota-low-first` is active and a high-priority account has more remaining quota than a lower-priority account
- **THEN** the lower-quota account is selected regardless of manual priority

#### Scenario: Exact automatic tie
- **WHEN** multiple accounts have the same complete automatic routing key
- **THEN** the scheduler rotates equally among them without applying manual weight

### Requirement: Strict priority selection
Within a strict profile's selected account layer and profile tier, the plugin SHALL consider only accounts having the highest numeric priority.

#### Scenario: Lower priority remains idle
- **WHEN** priority 100 and priority 80 accounts are both eligible in the active layer and profile tier
- **THEN** the scheduler selects only from priority 100 accounts

#### Scenario: Higher priority becomes unavailable
- **WHEN** all priority 100 accounts are excluded or already tried
- **THEN** the scheduler proceeds to the next lower eligible priority

#### Scenario: Plugin receives lower priorities before filtering
- **WHEN** multiple host-available accounts have different host priorities
- **THEN** the host supplies all of them to the plugin so plugin quota and backup filtering can choose the highest remaining eligible plugin priority

#### Scenario: Unhandled plugin preserves built-in priority
- **WHEN** the plugin leaves a scheduling request unhandled
- **THEN** the host applies its built-in highest-priority reduction before invoking the configured built-in selector

### Requirement: Weighted selection
The plugin SHALL use smooth weighted round-robin among accounts in the same active strict layer, profile tier, and priority.

#### Scenario: Three-to-one weighting
- **WHEN** two continuously eligible accounts have weights three and one in the same selection group
- **THEN** repeated new-session selections converge to a three-to-one distribution without random selection

#### Scenario: Invalid weight
- **WHEN** an account policy contains a weight below one
- **THEN** policy validation rejects the update

### Requirement: Strict session affinity
The plugin SHALL retain a session-to-account binding only while the bound account remains in the currently selected exact strict or automatic selection group.

#### Scenario: Affinity hit inside active group
- **WHEN** a session has a non-expired binding to an eligible account in the current strict selection group
- **THEN** the scheduler returns the bound account

#### Scenario: Derived session identity
- **WHEN** the host supplies a stable `derived_session_id` in scheduler metadata for a request without an explicit session header
- **THEN** the plugin uses that identity for the same strict-group affinity behavior

#### Scenario: Profile switch invalidates lower-tier affinity
- **WHEN** a session is bound to a paid account and the active profile changes to `free-first` while a Free account is eligible
- **THEN** the scheduler ignores the paid binding and creates a new binding in the Free tier

#### Scenario: Automatic score change invalidates affinity
- **WHEN** a session is bound under `quota-low-first` and a refreshed snapshot makes another eligible account the unique lowest-quota account
- **THEN** the scheduler ignores the old binding and selects from the new automatic group

#### Scenario: Bound account becomes unavailable
- **WHEN** the bound account is no longer an eligible candidate
- **THEN** the scheduler removes the binding and selects another account

#### Scenario: Affinity state remains bounded
- **WHEN** many unique sessions and policy revisions are observed over time
- **THEN** the plugin removes expired or mismatched affinity entries and discards weighted state from prior policy revisions

### Requirement: Retry-aware fallback
The plugin SHALL make each scheduling decision from the candidate list supplied by the host and SHALL not reselect an account omitted by the host after a failed attempt.

#### Scenario: Selected account fails
- **WHEN** the host retries a request with the failed account removed from candidates
- **THEN** the plugin selects the next eligible account according to the same strict ordering

#### Scenario: No eligible account
- **WHEN** the plugin owns a Codex scheduling request but no account is eligible
- **THEN** the plugin returns a handled scheduling error rather than delegating to a scheduler that ignores quota policy
