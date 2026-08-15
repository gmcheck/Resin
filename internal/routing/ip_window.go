package routing

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

// IPAccountWindow tracks, per egress IP, when each distinct account was first
// bound to that IP. It powers the "max distinct accounts per IP per rolling
// window" quota, which is enforced only when creating new sticky leases.
//
// Semantics:
//   - firstSeen is the account's first binding time on that IP; re-binding the
//     same account never refreshes it and never counts twice;
//   - entries survive lease deletion until they slide out of the window;
//   - quota state is per (platform, IP): each platform owns one window.
//
// All methods are safe for concurrent use.
type IPAccountWindow struct {
	mu      sync.Mutex
	entries map[netip.Addr]map[string]int64 // ip -> account -> firstSeenNs

	blockedTotal  atomic.Int64
	fallbackTotal atomic.Int64
}

// NewIPAccountWindow creates an empty window.
func NewIPAccountWindow() *IPAccountWindow {
	return &IPAccountWindow{
		entries: make(map[netip.Addr]map[string]int64),
	}
}

// Touch records an account→IP binding at firstSeenNs. Idempotent: an existing
// entry keeps its original firstSeen. Invalid inputs are ignored.
func (w *IPAccountWindow) Touch(ip netip.Addr, account string, firstSeenNs int64) {
	if !ip.IsValid() || account == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.touchLocked(ip, account, firstSeenNs)
}

func (w *IPAccountWindow) touchLocked(ip netip.Addr, account string, firstSeenNs int64) {
	m, ok := w.entries[ip]
	if !ok {
		m = make(map[string]int64)
		w.entries[ip] = m
	}
	if _, exists := m[account]; !exists {
		m[account] = firstSeenNs
	}
}

// countLocked prunes expired entries for ip and returns the number of distinct
// accounts bound within the window.
func (w *IPAccountWindow) countLocked(ip netip.Addr, nowNs, windowNs int64) int {
	m, ok := w.entries[ip]
	if !ok {
		return 0
	}
	cutoff := nowNs - windowNs
	count := 0
	for acc, seen := range m {
		if seen <= cutoff {
			delete(m, acc)
			continue
		}
		count++
	}
	if len(m) == 0 {
		delete(w.entries, ip)
	}
	return count
}

// CountRecent returns the number of distinct accounts bound to ip within the
// rolling window, pruning expired entries for that IP along the way.
func (w *IPAccountWindow) CountRecent(ip netip.Addr, nowNs, windowNs int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.countLocked(ip, nowNs, windowNs)
}

// CountsByIP returns distinct in-window account counts per IP, pruning expired
// entries across all IPs.
func (w *IPAccountWindow) CountsByIP(nowNs, windowNs int64) map[netip.Addr]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[netip.Addr]int, len(w.entries))
	for ip := range w.entries {
		if n := w.countLocked(ip, nowNs, windowNs); n > 0 {
			out[ip] = n
		}
	}
	return out
}

// ReserveIfEligible atomically checks the quota and reserves a slot for
// account on ip. It returns true when:
//   - the account is already counted for this IP (re-binding keeps its
//     original firstSeen and does not consume another slot), or
//   - the in-window distinct account count is below max (a new entry is
//     recorded at nowNs).
//
// Otherwise it returns false and increments the blocked counter (per blocked
// pick, so one request may add up to quotaPickAttempts). Check and touch
// happen under the same lock, so concurrent lease creations cannot overshoot
// the quota through the normal path.
func (w *IPAccountWindow) ReserveIfEligible(ip netip.Addr, account string, nowNs, windowNs int64, max int) bool {
	if !ip.IsValid() || account == "" || max <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	count := w.countLocked(ip, nowNs, windowNs)
	if _, counted := w.entries[ip][account]; counted {
		return true
	}
	if count >= max {
		w.blockedTotal.Add(1)
		return false
	}
	w.touchLocked(ip, account, nowNs)
	return true
}

// ReserveForcedFallback is the fail-open path used when every pick was blocked:
// it records account on ip even though the quota is full, subject to a hard
// ceiling (max + 1 distinct accounts per IP per window) so that overshoot stays
// bounded. Returns false when even the ceiling is reached.
func (w *IPAccountWindow) ReserveForcedFallback(ip netip.Addr, account string, nowNs, windowNs int64, max int) bool {
	if !ip.IsValid() || account == "" {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	count := w.countLocked(ip, nowNs, windowNs)
	if _, counted := w.entries[ip][account]; counted {
		return true
	}
	if count >= max+1 {
		return false
	}
	w.touchLocked(ip, account, nowNs)
	w.fallbackTotal.Add(1)
	return true
}

// Prune removes expired entries across all IPs.
func (w *IPAccountWindow) Prune(nowNs, windowNs int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := nowNs - windowNs
	for ip, m := range w.entries {
		for acc, seen := range m {
			if seen <= cutoff {
				delete(m, acc)
			}
		}
		if len(m) == 0 {
			delete(w.entries, ip)
		}
	}
}

// Stats returns the cumulative quota-blocked and forced-fallback counters.
func (w *IPAccountWindow) Stats() (blockedTotal, fallbackTotal int64) {
	return w.blockedTotal.Load(), w.fallbackTotal.Load()
}
