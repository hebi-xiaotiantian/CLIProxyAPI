# Codex Account Pool Automatic Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add automatic subscription/quota routing modes, operator-friendly scheduling diagnostics, and a bounded history of real Codex scheduling decisions without changing the existing strict priority, weight, backup, or affinity semantics.

**Architecture:** Keep routing metrics and exact-group construction inside the scheduler, with one shared analysis result used by both real picks and non-mutating previews. Store recent real decisions in a small concurrency-safe in-memory ring owned by the plugin; previews run through a cloned selector with recording disabled. Extend the embedded static management page to consume structured preview and decision APIs.

**Tech Stack:** Go 1.26+, CLIProxyAPI plugin ABI, standard library JSON/HTTP/synchronization, embedded HTML/CSS/JavaScript, OpenSpec.

---

### Task 1: Automatic Routing Metrics

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/model.go`
- Modify: `examples/plugin/codex-account-pool/go/scheduler.go`
- Test: `examples/plugin/codex-account-pool/go/scheduler_test.go`

- [ ] **Step 1: Write failing profile-validation and metric tests**

Add table-driven tests that require all new profile values to normalize successfully and invalid values to fail. Add focused tests for:

```go
func TestPlanRank(t *testing.T) {
	tests := []struct {
		raw  string
		rank int
		ok   bool
	}{
		{"free", 100, true},
		{"go", 200, true},
		{"plus", 300, true},
		{"team", 300, true},
		{"business", 300, true},
		{"pro", 400, true},
		{"prolite", 400, true},
		{"promax", 500, true},
		{"enterprise", 600, true},
		{"edu-enterprise", 600, true},
		{"mystery", 0, false},
	}
	for _, test := range tests {
		rank, ok := planRank(test.raw)
		if rank != test.rank || ok != test.ok {
			t.Fatalf("planRank(%q) = %d, %v; want %d, %v", test.raw, rank, ok, test.rank, test.ok)
		}
	}
}

func TestQuotaScoreUsesMinimumPresentWindow(t *testing.T) {
	score, ok := quotaScore(QuotaSnapshot{
		FiveHourRemaining:     70,
		WeeklyRemaining:       40,
		FiveHourWindowPresent: true,
		WeeklyWindowPresent:   true,
	})
	if score != 40 || !ok {
		t.Fatalf("quotaScore() = %d, %v; want 40, true", score, ok)
	}
}
```

Also cover five-hour-only, weekly-only, and no-window snapshots.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test -run 'Test(PlanRank|QuotaScore|NormalizePolicyDocumentAcceptsAutomaticProfiles)' ./...
```

Expected: FAIL because the automatic profile constants and metric helpers do not exist.

- [ ] **Step 3: Add automatic profile constants and metric helpers**

Add these `RouteProfile` values:

```go
ProfileAuto           RouteProfile = "auto"
ProfileQuotaHighFirst RouteProfile = "quota-high-first"
ProfileQuotaLowFirst  RouteProfile = "quota-low-first"
ProfilePlanHighFirst  RouteProfile = "plan-high-first"
ProfilePlanLowFirst   RouteProfile = "plan-low-first"
```

Extend `validProfile`, add `isAutomaticProfile`, normalize plan strings case-insensitively with punctuation removed, and implement:

```go
func planRank(raw string) (int, bool)
func quotaScore(snapshot QuotaSnapshot) (int, bool)
```

`quotaScore` returns the minimum remaining percentage across present windows and returns `ok=false` when neither window is present.

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run the Step 2 command again.

Expected: PASS.

### Task 2: Automatic Exact-Group Selection

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/scheduler.go`
- Test: `examples/plugin/codex-account-pool/go/scheduler_test.go`

- [ ] **Step 1: Write failing automatic-order tests**

Add scheduler tests that prove:

```go
func TestSchedulerAutoUsesPlanThenQuota(t *testing.T)
func TestSchedulerQuotaHighFirstUsesQuotaThenPlan(t *testing.T)
func TestSchedulerQuotaLowFirstUsesLowestEligibleQuota(t *testing.T)
func TestSchedulerPlanHighFirstUsesHighestPlanThenQuota(t *testing.T)
func TestSchedulerPlanLowFirstUsesLowestPlanThenQuota(t *testing.T)
func TestSchedulerAutomaticUnknownMetricsSortLast(t *testing.T)
func TestSchedulerAutomaticReserveFilteringPrecedesOrdering(t *testing.T)
func TestSchedulerAutomaticKeepsBackupIndependent(t *testing.T)
```

Use candidates whose manual priorities and weights intentionally disagree with the automatic ordering so each test proves those fields are ignored.

- [ ] **Step 2: Run automatic-order tests and verify RED**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test -run 'TestScheduler(Auto|Quota|Plan|Automatic)' ./...
```

Expected: FAIL because every selection still uses strict tiers and manual priority.

- [ ] **Step 3: Extend candidate classification**

Extend `selectionCandidate` with:

```go
PlanRank   int
PlanKnown  bool
QuotaScore int
QuotaKnown bool
```

Populate those fields from `QuotaSnapshot.PlanType` and presence-aware quota windows after all host/plugin/staleness/reserve filters have passed.

- [ ] **Step 4: Split strict and automatic group construction**

Introduce:

```go
func selectionGroup(profile RouteProfile, candidates []selectionCandidate) []selectionCandidate
func strictProfileGroup(candidates []selectionCandidate) []selectionCandidate
func automaticProfileGroup(profile RouteProfile, candidates []selectionCandidate) []selectionCandidate
```

All modes first choose regular candidates when any regular candidate is eligible, otherwise backups. Strict profiles retain tier then highest priority. Automatic profiles compare a complete presence-aware key:

```text
auto / plan-high-first: known plan desc, plan rank desc, known quota desc, quota desc
plan-low-first: known plan desc, plan rank asc, known quota desc, quota desc
quota-high-first: known quota desc, quota desc, known plan desc, plan rank desc
quota-low-first: known quota desc, quota asc, known plan desc, plan rank desc
```

Return every candidate tied on the full key, sorted by account ID.

- [ ] **Step 5: Run automatic-order tests and verify GREEN**

Run the Step 2 command again.

Expected: PASS.

### Task 3: Equal Rotation and Affinity

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/scheduler.go`
- Test: `examples/plugin/codex-account-pool/go/scheduler_test.go`

- [ ] **Step 1: Write failing tie and affinity tests**

Add:

```go
func TestSchedulerAutomaticExactTieRotatesEqually(t *testing.T)
func TestSchedulerAutomaticExactTieIgnoresWeight(t *testing.T)
func TestSchedulerAutomaticScoreChangeInvalidatesAffinity(t *testing.T)
func TestSchedulerStrictWeightedRoundRobinStillUsesWeight(t *testing.T)
```

For the tie test, give accounts weights `9` and `1`, run 20 new-session picks, and require a `10/10` result. For affinity invalidation, bind a session to one `quota-low-first` winner, update quota so another account becomes the unique winner without changing policy revision, and require rebinding.

- [ ] **Step 2: Run tie and affinity tests and verify RED**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test -run 'TestScheduler(AutomaticExactTie|AutomaticScoreChange|StrictWeighted)' ./...
```

Expected: FAIL because automatic ties still consume saved manual weights.

- [ ] **Step 3: Make selection state mode-aware**

Keep `weightedPick` unchanged for strict modes. Add an equal smooth rotation helper that gives every automatic tie weight one, and select it from `pick` when `isAutomaticProfile(profile)` is true. Include the exact ordered group IDs and profile in the rotation key so a quota-score change creates a new active group and invalidates affinity outside that group.

- [ ] **Step 4: Run scheduler tests and verify GREEN**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test ./...
```

Expected: PASS.

### Task 4: Structured Preview

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/management.go`
- Test: `examples/plugin/codex-account-pool/go/management_test.go`

- [ ] **Step 1: Write failing preview-schema tests**

Add tests that require the preview response to contain:

```go
type previewFunnelView struct {
	Candidates     int `json:"candidates"`
	HostAvailable  int `json:"host_available"`
	QuotaEligible  int `json:"quota_eligible"`
	ActiveLayer    int `json:"active_layer"`
	StrategyGroup  int `json:"strategy_group"`
}
```

Each candidate row must expose account label, plan type, layer, eligibility, human-readable reason code, plan rank presence/value, quota score presence/value, priority, weight, and whether it belongs to the active group. The response must include grouped exclusion counts and a selected-account summary. Existing preview clone behavior must still prove weight, rotation, and affinity state are not consumed.

- [ ] **Step 2: Run preview tests and verify RED**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test -run 'TestPreview' ./...
```

Expected: FAIL because the current response only contains raw candidate fields and aggregate count.

- [ ] **Step 3: Share scheduler analysis with preview**

Refactor scheduler preparation into a side-effect-free helper:

```go
type selectionAnalysis struct {
	Profile        RouteProfile
	CandidateCount int
	HostAvailable  int
	QuotaEligible  int
	Eligible       []selectionCandidate
	Group          []selectionCandidate
	Excluded       map[string]int
}

func analyzeSelection(req pluginapi.SchedulerPickRequest, policy PolicyDocument, quota QuotaDocument, cfg Config, now time.Time) (selectionAnalysis, error)
```

Use the same helper in `selector.pick` and `accountPoolPlugin.preview`. Preserve stable sorting and return sanitized reason codes suitable for UI labels.

- [ ] **Step 4: Build the structured preview response**

Return JSON fields:

```text
profile, automatic, model, selected_auth, selected_account,
active_layer, selection_group, funnel, exclusions, candidates
```

Do not include request bodies, credentials, tokens, or the supplied session identifier.

- [ ] **Step 5: Run management and scheduler tests**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test ./...
```

Expected: PASS.

### Task 5: Recent Real Decisions

**Files:**
- Create: `examples/plugin/codex-account-pool/go/decisions.go`
- Create: `examples/plugin/codex-account-pool/go/decisions_test.go`
- Modify: `examples/plugin/codex-account-pool/go/plugin.go`
- Modify: `examples/plugin/codex-account-pool/go/management.go`
- Modify: `examples/plugin/codex-account-pool/go/plugin_test.go`
- Test: `examples/plugin/codex-account-pool/go/management_test.go`

- [ ] **Step 1: Write failing decision-ring tests**

Add tests for newest-first snapshots, fixed-capacity eviction, copy isolation, and secret-free fields:

```go
type schedulingDecision struct {
	Timestamp      time.Time    `json:"timestamp"`
	Model          string       `json:"model,omitempty"`
	AuthID         string       `json:"auth_id"`
	Profile        RouteProfile `json:"profile"`
	Layer          string       `json:"layer"`
	CandidateCount int          `json:"candidate_count"`
	EligibleCount  int          `json:"eligible_count"`
	GroupSize      int          `json:"group_size"`
}
```

Also require real `accountPoolPlugin.pick` calls to add one entry and repeated previews to add none.

- [ ] **Step 2: Run decision tests and verify RED**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test -run 'Test(Decision|PreviewDoesNotRecord|PluginPickRecords)' ./...
```

Expected: FAIL because no decision store or route exists.

- [ ] **Step 3: Implement the bounded ring**

Implement a mutex-protected `decisionRing` with default capacity `100`:

```go
func newDecisionRing(capacity int) *decisionRing
func (r *decisionRing) add(decision schedulingDecision)
func (r *decisionRing) snapshot() []schedulingDecision
```

The ring stores only the explicit fields above and returns copied newest-first values.

- [ ] **Step 4: Record only successful real picks**

Have the selector return selection metadata to `accountPoolPlugin.pick`, or provide a callback invoked only by the real plugin pick path. Record after a handled successful selection. Do not attach recording to `selector.pick` itself if that would allow preview clones to write.

- [ ] **Step 5: Add the authenticated decisions route**

Register and handle:

```text
GET /codex-account-pool/decisions
```

Return:

```json
{"decisions":[]}
```

with the existing no-store JSON headers.

- [ ] **Step 6: Run decision and management tests**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test ./...
```

Expected: PASS.

### Task 6: Operator-Facing Management UI

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/web/index.html`
- Modify: `examples/plugin/codex-account-pool/go/management_test.go`

- [ ] **Step 1: Write failing static-resource contract tests**

Require stable element IDs for:

```text
automaticProfileSegments
strictProfileSegments
profileRule
previewFunnel
previewSelected
previewCandidates
previewExclusions
decisionRows
manualRoutingNotice
```

Require all five automatic profile values and the `/decisions` API path. Reject the old `JSON.stringify(..., null, 2)` preview rendering.

- [ ] **Step 2: Run the static-resource tests and verify RED**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test -run 'TestStaticResource' ./...
```

Expected: FAIL because the page exposes only strict modes and a raw JSON `<pre>`.

- [ ] **Step 3: Group route modes and explain the active rule**

Render two compact segmented-control groups:

```text
Automatic: Auto, High quota, Low quota, High subscription, Low subscription
Strict: Paid first, Free first, Free only, Custom
```

Show one short active-rule line. Disable priority and weight inputs and their batch controls while an automatic mode is effective, but keep values visible and untouched.

- [ ] **Step 4: Replace raw JSON preview**

Render:

1. a six-step route funnel;
2. selected account and active layer summary;
3. ordered candidate table with readable plan/quota/order/group fields;
4. grouped exclusion reason counts.

Use text mappings such as `quota_stale -> 额度数据过期`, `weekly_reserve -> 低于周额度保留线`, and `profile_excluded -> 不属于当前严格模式`.

- [ ] **Step 5: Render recent real scheduling**

Load `/decisions` alongside accounts and profile data. Render timestamp, model, account, mode, layer, candidate/eligible/group counts in a distinct table labelled as real routing history.

- [ ] **Step 6: Verify responsive and secret-free markup**

Use fixed table layouts with horizontal overflow, keep controls within viewport widths, and ensure the embedded HTML contains no account state, tokens, credentials, or server address.

- [ ] **Step 7: Run static-resource and plugin tests**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 go test ./...
```

Expected: PASS.

### Task 7: Documentation and Full Verification

**Files:**
- Modify: `examples/plugin/codex-account-pool/README.md`
- Modify: `openspec/changes/add-codex-account-pool-plugin/tasks.md`

- [ ] **Step 1: Update routing documentation**

Document every profile, the automatic metric precedence, unknown-last behavior, equal tie rotation, reserve-before-ordering, independent backup layer, and that automatic modes retain but ignore saved priority/weight.

- [ ] **Step 2: Format and run plugin verification**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src/examples/plugin/codex-account-pool/go golang:1.26 sh -lc 'gofmt -w . && go test ./... && go test -race ./... && go vet ./... && go build -buildmode=c-shared -o /tmp/codex-account-pool.so .'
```

Expected: all commands exit zero.

- [ ] **Step 3: Run host tests and required compile**

Run:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -lc 'go test ./... && go build -o /tmp/cli-proxy-api ./cmd/server'
```

Expected: all commands exit zero.

- [ ] **Step 4: Validate OpenSpec**

Run:

```bash
openspec validate add-codex-account-pool-plugin --strict
```

Expected: `Change 'add-codex-account-pool-plugin' is valid`.

- [ ] **Step 5: Inspect the final diff and mark OpenSpec tasks**

Review:

```bash
git diff --check
git status --short
git diff -- examples/plugin/codex-account-pool openspec/changes/add-codex-account-pool-plugin
```

Mark tasks `9.1` through `10.4` complete only when their focused tests pass. Mark `10.5` complete only after every local/container verification succeeds. Do not deploy or alter server configuration as part of this implementation.
