package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStableRefreshOffsetIsDeterministic(t *testing.T) {
	interval := 10 * time.Minute
	first := stableRefreshOffset("auth-a", interval)
	second := stableRefreshOffset("auth-a", interval)
	if first != second || first < 0 || first >= interval {
		t.Fatalf("offsets = %s/%s for interval %s", first, second, interval)
	}
}

func TestCoordinatorSuppressesDuplicateRefresh(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	refresher := func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return QuotaSnapshot{AuthID: "auth-a", RefreshedAt: time.Now()}, nil
	}
	coordinator := newRefreshCoordinator(2, refresher, func(QuotaSnapshot, error) {})
	defer coordinator.stop()
	account := pluginapi.HostAuthFileEntry{ID: "auth-a", AuthIndex: "index-a", Provider: "codex"}

	if !coordinator.queue(account) {
		t.Fatal("first queue() = false")
	}
	<-started
	if coordinator.queue(account) {
		t.Fatal("duplicate queue() = true")
	}
	close(release)
	coordinator.wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}

func TestCoordinatorHonorsConcurrencyLimit(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	refresher := func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return QuotaSnapshot{RefreshedAt: time.Now()}, nil
	}
	coordinator := newRefreshCoordinator(2, refresher, func(QuotaSnapshot, error) {})
	defer coordinator.stop()
	for _, id := range []string{"a", "b", "c", "d"} {
		if !coordinator.queue(pluginapi.HostAuthFileEntry{ID: id, Provider: "codex"}) {
			t.Fatalf("queue(%s) = false", id)
		}
	}
	<-started
	<-started
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maximum.Load())
	}
	close(release)
	coordinator.wait()
}

func TestDelayedRefreshIsDeduplicated(t *testing.T) {
	var calls atomic.Int32
	plugin := newTestPlugin(t)
	plugin.inventory = []pluginapi.HostAuthFileEntry{{
		ID: "auth-a", AuthIndex: "index-a", Provider: "codex",
	}}
	plugin.coordinator = newRefreshCoordinator(1, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		calls.Add(1)
		return QuotaSnapshot{AuthID: "auth-a", RefreshedAt: time.Now()}, nil
	}, func(QuotaSnapshot, error) {})
	defer plugin.shutdown()

	if !plugin.queueRefreshByID("auth-a", 20*time.Millisecond) {
		t.Fatal("first delayed queue = false")
	}
	if plugin.queueRefreshByID("auth-a", 20*time.Millisecond) {
		t.Fatal("duplicate delayed queue = true")
	}
	time.Sleep(80 * time.Millisecond)
	plugin.coordinator.wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}

func TestQueueRefreshFiltersByDetectedPlan(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.inventory = []pluginapi.HostAuthFileEntry{
		{ID: "free", AuthIndex: "free-index", Provider: "codex"},
		{ID: "paid", AuthIndex: "paid-index", Provider: "codex"},
	}
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"free": {AuthID: "free", Plan: PlanFree},
			"paid": {AuthID: "paid", Plan: PlanPaid},
		},
	})
	release := make(chan struct{})
	plugin.coordinator = newRefreshCoordinator(1, func(_ context.Context, account pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		<-release
		return QuotaSnapshot{AuthID: account.ID}, nil
	}, func(QuotaSnapshot, error) {})
	defer plugin.shutdown()

	queued := plugin.queueRefresh(nil, PlanFree)
	if len(queued) != 1 || queued[0] != "free" {
		t.Fatalf("queued = %#v, want free", queued)
	}
	close(release)
	plugin.coordinator.wait()
}

func TestCoordinatorPublishesRefreshStates(t *testing.T) {
	states := make(chan string, 2)
	release := make(chan struct{})
	coordinator := newRefreshCoordinator(1, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		<-release
		return QuotaSnapshot{AuthID: "auth-a"}, nil
	}, func(QuotaSnapshot, error) {})
	coordinator.setStateCallback(func(_ string, state string) {
		states <- state
	})
	defer coordinator.stop()

	if !coordinator.queue(pluginapi.HostAuthFileEntry{ID: "auth-a", Provider: "codex"}) {
		t.Fatal("queue() = false")
	}
	if got := <-states; got != "queued" {
		t.Fatalf("first state = %q, want queued", got)
	}
	if got := <-states; got != "refreshing" {
		t.Fatalf("second state = %q, want refreshing", got)
	}
	close(release)
	coordinator.wait()
}

func TestCoordinatorKeepsAccountInFlightUntilResultIsApplied(t *testing.T) {
	resultStarted := make(chan struct{}, 2)
	releaseResult := make(chan struct{})
	coordinator := newRefreshCoordinator(2, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		return QuotaSnapshot{AuthID: "auth-a"}, nil
	}, func(QuotaSnapshot, error) {
		resultStarted <- struct{}{}
		<-releaseResult
	})
	defer coordinator.stop()
	account := pluginapi.HostAuthFileEntry{ID: "auth-a", Provider: "codex"}

	if !coordinator.queue(account) {
		t.Fatal("first queue() = false")
	}
	<-resultStarted
	duplicateAccepted := coordinator.queue(account)
	close(releaseResult)
	coordinator.wait()
	if duplicateAccepted {
		t.Fatal("queue() accepted duplicate while the first result was still being applied")
	}
}

func TestCoordinatorRejectsWorkAfterStop(t *testing.T) {
	coordinator := newRefreshCoordinator(1, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		return QuotaSnapshot{AuthID: "auth-a"}, nil
	}, func(QuotaSnapshot, error) {})
	coordinator.stop()

	if coordinator.queue(pluginapi.HostAuthFileEntry{ID: "auth-a", Provider: "codex"}) {
		t.Fatal("queue() accepted work after stop")
	}
}

func TestCoordinatorExposesScheduledNextRefresh(t *testing.T) {
	coordinator := newRefreshCoordinator(1, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		return QuotaSnapshot{AuthID: "auth-a"}, nil
	}, func(QuotaSnapshot, error) {})
	defer coordinator.stop()
	coordinator.startSchedule(time.Hour, func() []pluginapi.HostAuthFileEntry {
		return []pluginapi.HostAuthFileEntry{{ID: "auth-a", Provider: "codex"}}
	})

	deadline := time.Now().Add(time.Second)
	for coordinator.nextRefresh("auth-a").IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	next := coordinator.nextRefresh("auth-a")
	if next.IsZero() || !next.After(time.Now()) {
		t.Fatalf("next refresh = %s", next)
	}
}

func TestCoordinatorStopCancelsActiveRefresh(t *testing.T) {
	started := make(chan struct{})
	coordinator := newRefreshCoordinator(1, func(ctx context.Context, _ pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		close(started)
		<-ctx.Done()
		return QuotaSnapshot{AuthID: "auth-a"}, ctx.Err()
	}, func(QuotaSnapshot, error) {})
	if !coordinator.queue(pluginapi.HostAuthFileEntry{ID: "auth-a", Provider: "codex"}) {
		t.Fatal("queue() = false")
	}
	<-started
	stopped := make(chan struct{})
	go func() {
		coordinator.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("coordinator stop timed out")
	}
}

func TestPluginShutdownCancelsDelayedRefresh(t *testing.T) {
	var calls atomic.Int32
	plugin := newTestPlugin(t)
	plugin.inventory = []pluginapi.HostAuthFileEntry{{
		ID: "auth-a", AuthIndex: "index-a", Provider: "codex",
	}}
	plugin.coordinator = newRefreshCoordinator(1, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		calls.Add(1)
		return QuotaSnapshot{AuthID: "auth-a"}, nil
	}, func(QuotaSnapshot, error) {})
	if !plugin.queueRefreshByID("auth-a", 20*time.Millisecond) {
		t.Fatal("delayed queue = false")
	}
	plugin.shutdown()
	time.Sleep(60 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("refresh calls after shutdown = %d", calls.Load())
	}
}
