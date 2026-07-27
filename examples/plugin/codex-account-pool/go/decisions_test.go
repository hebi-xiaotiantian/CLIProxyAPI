package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDecisionRingKeepsNewestEntriesAndReturnsCopies(t *testing.T) {
	ring := newDecisionRing(2)
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	ring.add(schedulingDecision{Timestamp: base, AuthID: "a"})
	ring.add(schedulingDecision{Timestamp: base.Add(time.Second), AuthID: "b"})
	ring.add(schedulingDecision{Timestamp: base.Add(2 * time.Second), AuthID: "c"})

	got := ring.snapshot()
	if len(got) != 2 || got[0].AuthID != "c" || got[1].AuthID != "b" {
		t.Fatalf("snapshot = %#v, want c then b", got)
	}
	got[0].AuthID = "changed"
	if next := ring.snapshot(); next[0].AuthID != "c" {
		t.Fatalf("snapshot mutation changed ring: %#v", next)
	}
}

func TestSchedulingDecisionJSONContainsNoSensitiveRequestFields(t *testing.T) {
	raw, err := json.Marshal(schedulingDecision{
		Timestamp:      time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		Model:          "gpt-5.4",
		AuthID:         "auth-a",
		Profile:        ProfileAuto,
		Layer:          "regular",
		CandidateCount: 4,
		EligibleCount:  3,
		GroupSize:      2,
	})
	if err != nil {
		t.Fatalf("marshal scheduling decision: %v", err)
	}
	text := strings.ToLower(string(raw))
	for _, forbidden := range []string{"session", "token", "credential", "request_body", "headers"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("decision JSON contains %q: %s", forbidden, text)
		}
	}
}
