package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed web/index.html
var accountPoolHTML []byte

type managementRegistrationResponse struct {
	Routes    []managementRouteDeclaration    `json:"routes,omitempty"`
	Resources []managementResourceDeclaration `json:"resources,omitempty"`
}

type managementRouteDeclaration struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementResourceDeclaration struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type AccountPolicyPatch struct {
	Enabled           *bool     `json:"enabled,omitempty"`
	Priority          *int      `json:"priority,omitempty"`
	Weight            *int      `json:"weight,omitempty"`
	Backup            *bool     `json:"backup,omitempty"`
	PlanOverride      *PlanKind `json:"plan_override,omitempty"`
	ClearPlanOverride bool      `json:"clear_plan_override,omitempty"`
	FiveHourReserve   *int      `json:"five_hour_reserve,omitempty"`
	WeeklyReserve     *int      `json:"weekly_reserve,omitempty"`
	Tags              *[]string `json:"tags,omitempty"`
}

type accountUpdate struct {
	ID  string             `json:"id"`
	Set AccountPolicyPatch `json:"set"`
}

type accountPatchRequest struct {
	IDs     []string           `json:"ids,omitempty"`
	Set     AccountPolicyPatch `json:"set,omitempty"`
	Updates []accountUpdate    `json:"updates,omitempty"`
}

type accountView struct {
	ID                string        `json:"id"`
	Label             string        `json:"label,omitempty"`
	Email             string        `json:"email,omitempty"`
	Status            string        `json:"status,omitempty"`
	StatusMessage     string        `json:"status_message,omitempty"`
	HostDisabled      bool          `json:"host_disabled"`
	HostUnavailable   bool          `json:"host_unavailable"`
	Plan              PlanKind      `json:"plan"`
	PlanType          string        `json:"plan_type,omitempty"`
	FiveHourRemaining int           `json:"five_hour_remaining"`
	WeeklyRemaining   int           `json:"weekly_remaining"`
	FiveHourPresent   bool          `json:"five_hour_window_present"`
	WeeklyPresent     bool          `json:"weekly_window_present"`
	FiveHourResetAt   time.Time     `json:"five_hour_reset_at,omitempty"`
	WeeklyResetAt     time.Time     `json:"weekly_reset_at,omitempty"`
	QuotaRefreshedAt  time.Time     `json:"quota_refreshed_at,omitempty"`
	QuotaFresh        bool          `json:"quota_fresh"`
	Refresh           RefreshStatus `json:"refresh,omitempty"`
	Policy            AccountPolicy `json:"policy"`
}

type previewCandidateView struct {
	ID       string   `json:"id"`
	Eligible bool     `json:"eligible"`
	Reason   string   `json:"reason,omitempty"`
	Layer    string   `json:"layer"`
	Plan     PlanKind `json:"plan"`
	Tier     int      `json:"tier"`
	Priority int      `json:"priority"`
	Weight   int      `json:"weight"`
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRouteDeclaration{
			{Method: http.MethodGet, Path: "/codex-account-pool/status"},
			{Method: http.MethodGet, Path: "/codex-account-pool/accounts"},
			{Method: http.MethodPatch, Path: "/codex-account-pool/accounts"},
			{Method: http.MethodGet, Path: "/codex-account-pool/profile"},
			{Method: http.MethodPut, Path: "/codex-account-pool/profile"},
			{Method: http.MethodPost, Path: "/codex-account-pool/refresh"},
			{Method: http.MethodPost, Path: "/codex-account-pool/preview"},
		},
		Resources: []managementResourceDeclaration{{
			Path:        "/pool",
			Menu:        "Codex Account Pool",
			Description: "Manage Codex account priority, weight, backup status, profiles, and quota refresh.",
		}},
	}
}

func (p *accountPoolPlugin) handleManagement(raw []byte) ([]byte, error) {
	var request managementRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	response, errHandle := p.managementResponse(request)
	if errHandle != nil {
		response = jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": sanitizeText(errHandle.Error()),
		})
	}
	return okEnvelope(response)
}

func (p *accountPoolPlugin) managementResponse(request managementRequest) (pluginapi.ManagementResponse, error) {
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	path := strings.TrimRight(strings.TrimSpace(request.Path), "/")
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, "/pool"):
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/html; charset=utf-8"}},
			Body:       append([]byte(nil), accountPoolHTML...),
		}, nil
	case method == http.MethodGet && strings.HasSuffix(path, "/codex-account-pool/status"):
		return jsonManagementResponse(http.StatusOK, p.statusView()), nil
	case method == http.MethodGet && strings.HasSuffix(path, "/codex-account-pool/accounts"):
		if errSync := p.syncInventory(); errSync != nil {
			p.setStatusError(errSync.Error())
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"accounts": p.accountViews()}), nil
	case method == http.MethodPatch && strings.HasSuffix(path, "/codex-account-pool/accounts"):
		var patch accountPatchRequest
		if errDecode := json.Unmarshal(request.Body, &patch); errDecode != nil {
			return pluginapi.ManagementResponse{}, fmt.Errorf("decode account patch: %w", errDecode)
		}
		if errApply := p.applyAccountUpdates(patch); errApply != nil {
			return pluginapi.ManagementResponse{}, errApply
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"accounts": p.accountViews()}), nil
	case method == http.MethodGet && strings.HasSuffix(path, "/codex-account-pool/profile"):
		return jsonManagementResponse(http.StatusOK, p.profileView()), nil
	case method == http.MethodPut && strings.HasSuffix(path, "/codex-account-pool/profile"):
		var input struct {
			Profile         RouteProfile `json:"profile"`
			DurationMinutes int          `json:"duration_minutes,omitempty"`
			ClearTemporary  bool         `json:"clear_temporary,omitempty"`
			CustomPlanOrder []PlanKind   `json:"custom_plan_order,omitempty"`
		}
		if errDecode := json.Unmarshal(request.Body, &input); errDecode != nil {
			return pluginapi.ManagementResponse{}, fmt.Errorf("decode profile update: %w", errDecode)
		}
		if input.ClearTemporary {
			if errClear := p.clearTemporaryProfile(); errClear != nil {
				return pluginapi.ManagementResponse{}, errClear
			}
		} else {
			var expiresAt *time.Time
			if input.DurationMinutes > 0 {
				value := time.Now().Add(time.Duration(input.DurationMinutes) * time.Minute).UTC()
				expiresAt = &value
			}
			if errSet := p.setProfileWithOrder(input.Profile, expiresAt, input.CustomPlanOrder); errSet != nil {
				return pluginapi.ManagementResponse{}, errSet
			}
		}
		return jsonManagementResponse(http.StatusOK, p.profileView()), nil
	case method == http.MethodPost && strings.HasSuffix(path, "/codex-account-pool/refresh"):
		var input struct {
			IDs  []string `json:"ids,omitempty"`
			Plan PlanKind `json:"plan,omitempty"`
		}
		if len(request.Body) > 0 {
			if errDecode := json.Unmarshal(request.Body, &input); errDecode != nil {
				return pluginapi.ManagementResponse{}, fmt.Errorf("decode refresh request: %w", errDecode)
			}
		}
		if input.Plan != "" && !validPlan(input.Plan) {
			return pluginapi.ManagementResponse{}, fmt.Errorf("invalid refresh plan %q", input.Plan)
		}
		queued := p.queueRefresh(input.IDs, input.Plan)
		return jsonManagementResponse(http.StatusAccepted, map[string]any{"queued": queued}), nil
	case method == http.MethodPost && strings.HasSuffix(path, "/codex-account-pool/preview"):
		var input struct {
			Model     string `json:"model,omitempty"`
			SessionID string `json:"session_id,omitempty"`
		}
		if len(request.Body) > 0 {
			if errDecode := json.Unmarshal(request.Body, &input); errDecode != nil {
				return pluginapi.ManagementResponse{}, fmt.Errorf("decode preview request: %w", errDecode)
			}
		}
		preview, errPreview := p.preview(input.Model, input.SessionID)
		if errPreview != nil {
			return jsonManagementResponse(http.StatusConflict, map[string]any{"error": sanitizeText(errPreview.Error())}), nil
		}
		return jsonManagementResponse(http.StatusOK, preview), nil
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]any{"error": "not_found"}), nil
	}
}

func (p *accountPoolPlugin) accountViews() []accountView {
	accounts := p.inventorySnapshot()
	policy := p.policy.Load()
	quota := p.quota.Load()
	p.configMu.RLock()
	cfg := p.config
	coordinator := p.coordinator
	p.configMu.RUnlock()
	now := time.Now()
	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		accountPolicy, ok := policy.Accounts[account.ID]
		if !ok {
			accountPolicy = defaultAccountPolicy(account.Priority)
		}
		snapshot := quota.Accounts[account.ID]
		plan := accountPolicy.PlanOverride
		if plan == "" {
			plan = snapshot.Plan
		}
		if !validPlan(plan) {
			plan = PlanUnknown
		}
		refresh := snapshot.Refresh
		if coordinator != nil {
			refresh.NextRefresh = coordinator.nextRefresh(account.ID)
		}
		views = append(views, accountView{
			ID:                account.ID,
			Label:             account.Label,
			Email:             account.Email,
			Status:            account.Status,
			StatusMessage:     account.StatusMessage,
			HostDisabled:      account.Disabled,
			HostUnavailable:   account.Unavailable,
			Plan:              plan,
			PlanType:          snapshot.PlanType,
			FiveHourRemaining: snapshot.FiveHourRemaining,
			WeeklyRemaining:   snapshot.WeeklyRemaining,
			FiveHourPresent:   snapshot.FiveHourWindowPresent,
			WeeklyPresent:     snapshot.WeeklyWindowPresent,
			FiveHourResetAt:   snapshot.FiveHourResetAt,
			WeeklyResetAt:     snapshot.WeeklyResetAt,
			QuotaRefreshedAt:  snapshot.RefreshedAt,
			QuotaFresh: (snapshot.FiveHourWindowPresent || snapshot.WeeklyWindowPresent) &&
				!snapshot.RefreshedAt.IsZero() && now.Sub(snapshot.RefreshedAt) <= cfg.SnapshotMaxAge,
			Refresh: refresh,
			Policy:  accountPolicy,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Policy.Backup != views[j].Policy.Backup {
			return !views[i].Policy.Backup
		}
		if views[i].Policy.Priority != views[j].Policy.Priority {
			return views[i].Policy.Priority > views[j].Policy.Priority
		}
		return views[i].ID < views[j].ID
	})
	return views
}

func (p *accountPoolPlugin) applyAccountPatch(ids []string, patch AccountPolicyPatch) error {
	return p.applyAccountUpdates(accountPatchRequest{IDs: ids, Set: patch})
}

func (p *accountPoolPlugin) applyAccountUpdates(request accountPatchRequest) error {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	current := p.policy.Load()
	next := clonePolicyDocument(*current)
	if len(request.IDs) == 0 && len(request.Updates) == 0 {
		return fmt.Errorf("at least one account update is required")
	}
	for _, rawID := range request.IDs {
		id := strings.TrimSpace(rawID)
		policy, ok := next.Accounts[id]
		if !ok {
			return fmt.Errorf("unknown account %q", id)
		}
		next.Accounts[id] = applyPolicyPatch(policy, request.Set)
	}
	for _, update := range request.Updates {
		id := strings.TrimSpace(update.ID)
		policy, ok := next.Accounts[id]
		if !ok {
			return fmt.Errorf("unknown account %q", id)
		}
		next.Accounts[id] = applyPolicyPatch(policy, update.Set)
	}
	next.Revision++
	normalized, errNormalize := normalizePolicyDocument(next)
	if errNormalize != nil {
		return errNormalize
	}
	p.configMu.RLock()
	store := p.store
	p.configMu.RUnlock()
	if store == nil {
		return fmt.Errorf("policy store is unavailable")
	}
	if errSave := store.savePolicy(normalized); errSave != nil {
		return errSave
	}
	p.statusMu.Lock()
	p.policyDegraded = false
	p.statusMu.Unlock()
	p.policy.Store(&normalized)
	return nil
}

func applyPolicyPatch(policy AccountPolicy, patch AccountPolicyPatch) AccountPolicy {
	if patch.Enabled != nil {
		policy.Enabled = *patch.Enabled
	}
	if patch.Priority != nil {
		policy.Priority = *patch.Priority
	}
	if patch.Weight != nil {
		policy.Weight = *patch.Weight
	}
	if patch.Backup != nil {
		policy.Backup = *patch.Backup
	}
	if patch.PlanOverride != nil {
		policy.PlanOverride = *patch.PlanOverride
	}
	if patch.ClearPlanOverride {
		policy.PlanOverride = ""
	}
	if patch.FiveHourReserve != nil {
		policy.FiveHourReserve = *patch.FiveHourReserve
		policy.ReserveDefaultsForPlan = ""
	}
	if patch.WeeklyReserve != nil {
		policy.WeeklyReserve = *patch.WeeklyReserve
		policy.ReserveDefaultsForPlan = ""
	}
	if patch.Tags != nil {
		policy.Tags = append([]string(nil), (*patch.Tags)...)
	}
	return policy
}

func (p *accountPoolPlugin) setProfile(profile RouteProfile, expiresAt *time.Time) error {
	return p.setProfileWithOrder(profile, expiresAt, nil)
}

func (p *accountPoolPlugin) setProfileWithOrder(profile RouteProfile, expiresAt *time.Time, customOrder []PlanKind) error {
	if !validProfile(profile) {
		return fmt.Errorf("invalid profile %q", profile)
	}
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	current := p.policy.Load()
	next := clonePolicyDocument(*current)
	if len(customOrder) > 0 {
		normalizedOrder, errOrder := normalizePlanOrder(customOrder)
		if errOrder != nil {
			return errOrder
		}
		next.CustomPlanOrder = normalizedOrder
	}
	if expiresAt != nil {
		if !expiresAt.After(time.Now()) {
			return fmt.Errorf("temporary profile expiration must be in the future")
		}
		next.TemporaryOverride = &ProfileOverride{Profile: profile, ExpiresAt: expiresAt.UTC()}
	} else {
		next.ActiveProfile = profile
		next.TemporaryOverride = nil
	}
	return p.persistPolicy(next)
}

func (p *accountPoolPlugin) clearTemporaryProfile() error {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	next := clonePolicyDocument(*p.policy.Load())
	next.TemporaryOverride = nil
	return p.persistPolicy(next)
}

func (p *accountPoolPlugin) persistPolicy(next PolicyDocument) error {
	next.Revision++
	normalized, errNormalize := normalizePolicyDocument(next)
	if errNormalize != nil {
		return errNormalize
	}
	p.configMu.RLock()
	store := p.store
	p.configMu.RUnlock()
	if store == nil {
		return fmt.Errorf("policy store is unavailable")
	}
	if errSave := store.savePolicy(normalized); errSave != nil {
		return errSave
	}
	p.statusMu.Lock()
	p.policyDegraded = false
	p.statusMu.Unlock()
	p.policy.Store(&normalized)
	return nil
}

func (p *accountPoolPlugin) queueRefresh(ids []string, plan PlanKind) []string {
	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if value := strings.TrimSpace(id); value != "" {
			selected[value] = struct{}{}
		}
	}
	quota := p.quota.Load()
	queued := make([]string, 0)
	for _, account := range p.inventorySnapshot() {
		if len(selected) > 0 {
			if _, ok := selected[account.ID]; !ok {
				continue
			}
		}
		if plan != "" {
			snapshot := quota.Accounts[account.ID]
			if snapshot.Plan != plan {
				continue
			}
		}
		if p.queueRefreshByID(account.ID, 0) {
			queued = append(queued, account.ID)
		}
	}
	sort.Strings(queued)
	return queued
}

func (p *accountPoolPlugin) preview(model, sessionID string) (map[string]any, error) {
	request := pluginapi.SchedulerPickRequest{Provider: "codex", Model: strings.TrimSpace(model)}
	if sessionID != "" {
		headers := http.Header{}
		headers.Set("X-Session-ID", sessionID)
		request.Options.Headers = headers
	}
	for _, account := range p.inventorySnapshot() {
		status := account.Status
		if account.Disabled {
			status = "disabled"
		} else if account.Unavailable {
			status = "unavailable"
		} else if strings.TrimSpace(status) == "" {
			status = "active"
		}
		request.Candidates = append(request.Candidates, pluginapi.SchedulerAuthCandidate{
			ID:       account.ID,
			Provider: "codex",
			Priority: account.Priority,
			Status:   status,
		})
	}
	p.configMu.RLock()
	cfg := p.config
	p.configMu.RUnlock()
	policy := *p.policy.Load()
	quota := *p.quota.Load()
	now := time.Now()
	profile := effectiveProfile(policy, now)
	eligible := make([]selectionCandidate, 0, len(request.Candidates))
	candidates := make([]previewCandidateView, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		accountPolicy, ok := policy.Accounts[candidate.ID]
		if !ok {
			accountPolicy = defaultAccountPolicy(candidate.Priority)
		}
		item, selectable, reason := classifyCandidateWithReason(
			candidate,
			accountPolicy,
			quota.Accounts[candidate.ID],
			profile,
			policy.CustomPlanOrder,
			cfg,
			now,
		)
		plan := accountPolicy.PlanOverride
		if plan == "" {
			plan = quota.Accounts[candidate.ID].Plan
		}
		if !validPlan(plan) {
			plan = PlanUnknown
		}
		view := previewCandidateView{
			ID:       candidate.ID,
			Eligible: selectable,
			Reason:   reason,
			Layer:    "regular",
			Plan:     plan,
			Priority: accountPolicy.Priority,
			Weight:   accountPolicy.Weight,
		}
		if accountPolicy.Backup {
			view.Layer = "backup"
		}
		if selectable {
			view.Tier = item.Tier
			eligible = append(eligible, item)
		}
		candidates = append(candidates, view)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Eligible != candidates[j].Eligible {
			return candidates[i].Eligible
		}
		if candidates[i].Layer != candidates[j].Layer {
			return candidates[i].Layer == "regular"
		}
		if candidates[i].Tier != candidates[j].Tier {
			return candidates[i].Tier > candidates[j].Tier
		}
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(eligible) == 0 {
		return nil, fmt.Errorf("codex account pool has no eligible account")
	}
	group := strictGroup(eligible)
	groupIDs := make([]string, 0, len(group))
	for _, candidate := range group {
		groupIDs = append(groupIDs, candidate.ID)
	}
	previewSelector := p.selector.clone()
	response, errPick := previewSelector.pick(request, policy, quota, cfg)
	if errPick != nil {
		return nil, errPick
	}
	return map[string]any{
		"profile":         profile,
		"selected_auth":   response.AuthID,
		"candidate_count": len(request.Candidates),
		"active_layer":    map[bool]string{true: "backup", false: "regular"}[group[0].Backup],
		"active_tier":     group[0].Tier,
		"active_priority": group[0].Priority,
		"selection_group": groupIDs,
		"candidates":      candidates,
	}, nil
}

func (p *accountPoolPlugin) profileView() map[string]any {
	policy := p.policy.Load()
	now := time.Now()
	var temporary any = policy.TemporaryOverride
	if policy.TemporaryOverride != nil && !now.Before(policy.TemporaryOverride.ExpiresAt) {
		temporary = nil
	}
	return map[string]any{
		"persistent":        policy.ActiveProfile,
		"effective":         effectiveProfile(*policy, now),
		"temporary":         temporary,
		"custom_plan_order": policy.CustomPlanOrder,
		"revision":          policy.Revision,
	}
}

func (p *accountPoolPlugin) statusView() map[string]any {
	p.configMu.RLock()
	cfg := p.config
	p.configMu.RUnlock()
	return map[string]any{
		"plugin":            pluginID,
		"version":           "0.1.0",
		"accounts":          len(p.inventorySnapshot()),
		"effective_profile": effectiveProfile(*p.policy.Load(), time.Now()),
		"refresh_interval":  cfg.RefreshInterval.String(),
		"snapshot_max_age":  cfg.SnapshotMaxAge.String(),
		"stale_policy":      cfg.StalePolicy,
		"state_error":       p.getStatusError(),
		"policy_degraded":   p.isPolicyDegraded(),
	}
}

func (p *accountPoolPlugin) isPolicyDegraded() bool {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.policyDegraded
}

func jsonManagementResponse(status int, value any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		raw = []byte(`{"error":"encode_response"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: raw,
	}
}
