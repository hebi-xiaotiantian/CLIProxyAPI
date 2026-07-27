package main

import (
	"context"
	"hash/fnv"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type refreshFunc func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error)

type refreshCoordinator struct {
	ctx       context.Context
	cancel    context.CancelFunc
	sem       chan struct{}
	refresh   refreshFunc
	onResult  func(QuotaSnapshot, error)
	onState   func(string, string)
	mu        sync.Mutex
	stopped   bool
	inFlight  map[string]struct{}
	next      map[string]time.Time
	workers   sync.WaitGroup
	scheduler sync.WaitGroup
}

func newRefreshCoordinator(concurrency int, refresh refreshFunc, onResult func(QuotaSnapshot, error)) *refreshCoordinator {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &refreshCoordinator{
		ctx:      ctx,
		cancel:   cancel,
		sem:      make(chan struct{}, concurrency),
		refresh:  refresh,
		onResult: onResult,
		inFlight: make(map[string]struct{}),
		next:     make(map[string]time.Time),
	}
}

func (c *refreshCoordinator) setStateCallback(callback func(string, string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onState = callback
	c.mu.Unlock()
}

func (c *refreshCoordinator) queue(account pluginapi.HostAuthFileEntry) bool {
	if c == nil || c.refresh == nil || account.ID == "" {
		return false
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return false
	}
	if _, exists := c.inFlight[account.ID]; exists {
		c.mu.Unlock()
		return false
	}
	c.inFlight[account.ID] = struct{}{}
	c.workers.Add(1)
	c.mu.Unlock()
	c.publishState(account.ID, "queued")

	go func() {
		defer c.workers.Done()
		defer c.finish(account.ID)
		select {
		case <-c.ctx.Done():
			return
		case c.sem <- struct{}{}:
		}
		c.publishState(account.ID, "refreshing")
		snapshot, errRefresh := c.refresh(c.ctx, account)
		if snapshot.AuthID == "" {
			snapshot.AuthID = account.ID
		}
		<-c.sem
		if c.onResult != nil {
			c.onResult(snapshot, errRefresh)
		}
	}()
	return true
}

func (c *refreshCoordinator) startSchedule(interval time.Duration, accounts func() []pluginapi.HostAuthFileEntry) {
	if c == nil || interval <= 0 || accounts == nil {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.scheduler.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.scheduler.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			now := time.Now()
			currentAccounts := accounts()
			active := make(map[string]struct{}, len(currentAccounts))
			for _, account := range currentAccounts {
				active[account.ID] = struct{}{}
				due, ok := c.nextRefreshValue(account.ID)
				if !ok {
					due = now.Add(stableRefreshOffset(account.ID, interval))
					c.setNextRefresh(account.ID, due)
				}
				if !now.Before(due) {
					if c.queue(account) {
						c.setNextRefresh(account.ID, now.Add(interval))
					}
				}
			}
			c.removeMissingNextRefresh(active)
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (c *refreshCoordinator) publishState(authID, state string) {
	c.mu.Lock()
	callback := c.onState
	c.mu.Unlock()
	if callback != nil {
		callback(authID, state)
	}
}

func (c *refreshCoordinator) nextRefreshValue(authID string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next, ok := c.next[authID]
	return next, ok
}

func (c *refreshCoordinator) setNextRefresh(authID string, next time.Time) {
	c.mu.Lock()
	c.next[authID] = next
	c.mu.Unlock()
}

func (c *refreshCoordinator) removeMissingNextRefresh(active map[string]struct{}) {
	c.mu.Lock()
	for authID := range c.next {
		if _, ok := active[authID]; !ok {
			delete(c.next, authID)
		}
	}
	c.mu.Unlock()
}

func (c *refreshCoordinator) nextRefresh(authID string) time.Time {
	next, _ := c.nextRefreshValue(authID)
	return next
}

func (c *refreshCoordinator) finish(authID string) {
	c.mu.Lock()
	delete(c.inFlight, authID)
	c.mu.Unlock()
}

func (c *refreshCoordinator) wait() {
	if c != nil {
		c.workers.Wait()
	}
}

func (c *refreshCoordinator) stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		c.cancel()
	}
	c.mu.Unlock()
	c.scheduler.Wait()
	c.workers.Wait()
}

func stableRefreshOffset(authID string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(authID))
	return time.Duration(hash.Sum64() % uint64(interval))
}
