package main

import (
	"sync"
	"time"
)

const maxRecentDecisions = 100

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

type decisionRing struct {
	mu      sync.Mutex
	entries []schedulingDecision
	next    int
	count   int
}

func newDecisionRing(capacity int) *decisionRing {
	if capacity < 1 {
		capacity = maxRecentDecisions
	}
	return &decisionRing{entries: make([]schedulingDecision, capacity)}
}

func (r *decisionRing) add(decision schedulingDecision) {
	if r == nil || len(r.entries) == 0 {
		return
	}
	r.mu.Lock()
	r.entries[r.next] = decision
	r.next = (r.next + 1) % len(r.entries)
	if r.count < len(r.entries) {
		r.count++
	}
	r.mu.Unlock()
}

func (r *decisionRing) clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	clear(r.entries)
	r.next = 0
	r.count = 0
	r.mu.Unlock()
}

func (r *decisionRing) snapshot() []schedulingDecision {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]schedulingDecision, 0, r.count)
	for offset := 0; offset < r.count; offset++ {
		index := (r.next - 1 - offset + len(r.entries)) % len(r.entries)
		out = append(out, r.entries[index])
	}
	return out
}
