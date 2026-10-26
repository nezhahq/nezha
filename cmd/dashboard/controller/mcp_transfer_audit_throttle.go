package controller

import (
	"sync"
	"time"
)

// transferAnonAuditThrottle caps the number of audit rows written per
// source IP within a sliding window for transfer requests that failed
// before a valid `entry` could be loaded (bogus/expired/replayed token).
//
// Without this cap an unauthenticated attacker can POST millions of
// /mcp/upload/<random> requests; every miss invokes
// writeTransferFailureAudit which inserts into mcp_audit_log. The
// throttle keeps a small per-IP token bucket in memory and drops audit
// rows past the budget — successful and authenticated failures (entry
// != nil) bypass this gate entirely so SIEM signal is unaffected.
type transferAnonAuditThrottle struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	global anonHitBucket
	hits   map[string]*anonHitBucket
	clock  func() time.Time
}

type anonHitBucket struct {
	firstAt time.Time
	count   int
}

func newTransferAnonAuditThrottle(window time.Duration, perWindow int) *transferAnonAuditThrottle {
	return &transferAnonAuditThrottle{
		window: window,
		limit:  perWindow,
		hits:   make(map[string]*anonHitBucket),
		clock:  time.Now,
	}
}

// shouldRecord reports whether the anonymous failure for this IP should
// land in the audit table. Unknown sources share one bucket.
func (t *transferAnonAuditThrottle) shouldRecord(ip string) bool {
	// Missing source information must consume a shared budget, never bypass it.
	if ip == "" {
		ip = "unknown-peer"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	t.pruneLocked(now)
	// Distributed sources share a hard write budget; entries cannot grow without bound.
	if t.global.firstAt.IsZero() || now.Sub(t.global.firstAt) >= t.window {
		t.global = anonHitBucket{firstAt: now}
	}
	if t.global.count >= 60 {
		return false
	}

	b, ok := t.hits[ip]
	if !ok || now.Sub(b.firstAt) >= t.window {
		t.global.count++
		t.hits[ip] = &anonHitBucket{firstAt: now, count: 1}
		return true
	}
	if b.count >= t.limit {
		return false
	}
	t.global.count++
	b.count++
	return true
}

func (t *transferAnonAuditThrottle) pruneLocked(now time.Time) {
	for ip, b := range t.hits {
		if now.Sub(b.firstAt) >= t.window {
			delete(t.hits, ip)
		}
	}
}

var transferAnonAuditThrottleShared = newTransferAnonAuditThrottle(time.Minute, 5)
