package routing

import (
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
)

// IPAccountWindow tracks, per egress IP, when each distinct account was last
// active on that IP. It powers the "max distinct accounts per IP per rolling
// window" quota, which bounds how many accounts an egress IP is exposed to.
//
// Semantics (recent-activity / lastSeen):
//   - an entry is refreshed on every lease hit, same-IP rotation and new
//     binding, so an actively used account keeps occupying its slot;
//   - an account that stops being served slides out of the window after
//     ip_account_window, releasing its slot for other accounts;
//   - a returning account (lease still alive but entry slid out) is
//     re-admitted through RefreshOnHit: below max as a regular slot, up to the
//     max+1 fallback ceiling, and beyond that the caller must rebuild the
//     lease elsewhere;
//   - entries survive lease deletion until they slide out of the window (an IP
//     stays "exposed" to the account for the rest of the window);
//   - quota state is per (platform, IP): each platform owns one window.
//
// All methods are safe for concurrent use.
type IPAccountWindow struct {
	mu      sync.Mutex
	entries map[netip.Addr]map[string]windowEntry // ip -> account -> entry

	blockedTotal  atomic.Int64
	fallbackTotal atomic.Int64
}

// windowEntry records the last activity time of an account on an IP and
// whether the account currently occupies a forced-fallback (max+1) slot.
type windowEntry struct {
	lastSeenNs  int64
	viaFallback bool
}

// IPAccountEntry is the per-account detail exported for observability.
// HasLease is filled in by the router snapshot (not by the window itself).
type IPAccountEntry struct {
	Account     string
	LastSeenNs  int64
	ViaFallback bool
	HasLease    bool
}

// NewIPAccountWindow creates an empty window.
func NewIPAccountWindow() *IPAccountWindow {
	return &IPAccountWindow{
		entries: make(map[netip.Addr]map[string]windowEntry),
	}
}

// Touch records an account activity on an IP at nowNs, inserting or refreshing
// the entry (refresh keeps the viaFallback flag). Invalid inputs are ignored.
func (w *IPAccountWindow) Touch(ip netip.Addr, account string, nowNs int64) {
	if !ip.IsValid() || account == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.touchLocked(ip, account, nowNs)
}

func (w *IPAccountWindow) touchLocked(ip netip.Addr, account string, nowNs int64) {
	m, ok := w.entries[ip]
	if !ok {
		m = make(map[string]windowEntry)
		w.entries[ip] = m
	}
	e, exists := m[account]
	if !exists {
		m[account] = windowEntry{lastSeenNs: nowNs}
		return
	}
	e.lastSeenNs = nowNs
	m[account] = e
}

// insertLocked records a (possibly new) entry, ensuring the per-IP map exists
// (countLocked may have removed it after pruning all expired entries).
func (w *IPAccountWindow) insertLocked(ip netip.Addr, account string, nowNs int64, viaFallback bool) {
	m, ok := w.entries[ip]
	if !ok {
		m = make(map[string]windowEntry)
		w.entries[ip] = m
	}
	m[account] = windowEntry{lastSeenNs: nowNs, viaFallback: viaFallback}
}

// countLocked prunes expired entries for ip and returns the number of distinct
// accounts active within the window.
func (w *IPAccountWindow) countLocked(ip netip.Addr, nowNs, windowNs int64) int {
	m, ok := w.entries[ip]
	if !ok {
		return 0
	}
	cutoff := nowNs - windowNs
	count := 0
	for acc, e := range m {
		if e.lastSeenNs <= cutoff {
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

// CountRecent returns the number of distinct accounts active on ip within the
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

// AccountsByIP returns in-window account details per IP (sorted by account),
// pruning expired entries across all IPs.
func (w *IPAccountWindow) AccountsByIP(nowNs, windowNs int64) map[netip.Addr][]IPAccountEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[netip.Addr][]IPAccountEntry, len(w.entries))
	for ip := range w.entries {
		if n := w.countLocked(ip, nowNs, windowNs); n > 0 {
			accounts := make([]IPAccountEntry, 0, n)
			for acc, e := range w.entries[ip] {
				accounts = append(accounts, IPAccountEntry{
					Account:     acc,
					LastSeenNs:  e.lastSeenNs,
					ViaFallback: e.viaFallback,
				})
			}
			sort.Slice(accounts, func(i, j int) bool { return accounts[i].Account < accounts[j].Account })
			out[ip] = accounts
		}
	}
	return out
}

// ReserveIfEligible atomically checks the quota and reserves a slot for
// account on ip. It returns true when:
//   - the account is already counted for this IP (its entry is refreshed), or
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
		w.touchLocked(ip, account, nowNs)
		return true
	}
	if count >= max {
		w.blockedTotal.Add(1)
		return false
	}
	w.touchLocked(ip, account, nowNs)
	return true
}

// RefreshOnHit is the admission path for existing sticky leases (lease hit and
// same-IP rotation). It keeps the invariant that an actively used account
// always occupies a window slot:
//
//   - entry present  -> refresh lastSeen, return true;
//   - entry absent   -> re-admit below max as a regular slot, up to max+1 as a
//     forced-fallback slot (counted in fallbackTotal), return true;
//   - at the max+1 ceiling -> return false: the caller must treat the lease as
//     invalid and rebuild it (which goes through normal quota selection).
//
// When the quota is disabled (max <= 0) it always returns true.
func (w *IPAccountWindow) RefreshOnHit(ip netip.Addr, account string, nowNs, windowNs int64, max int) bool {
	if !ip.IsValid() || account == "" || max <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	count := w.countLocked(ip, nowNs, windowNs)
	if e, counted := w.entries[ip][account]; counted {
		e.lastSeenNs = nowNs
		w.entries[ip][account] = e
		return true
	}
	if count >= max+1 {
		w.blockedTotal.Add(1)
		return false
	}
	viaFallback := count >= max
	if viaFallback {
		w.fallbackTotal.Add(1)
	}
	w.insertLocked(ip, account, nowNs, viaFallback)
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
		w.touchLocked(ip, account, nowNs)
		return true
	}
	if count >= max+1 {
		return false
	}
	w.insertLocked(ip, account, nowNs, true)
	w.fallbackTotal.Add(1)
	return true
}

// Prune removes expired entries across all IPs.
func (w *IPAccountWindow) Prune(nowNs, windowNs int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := nowNs - windowNs
	for ip, m := range w.entries {
		for acc, e := range m {
			if e.lastSeenNs <= cutoff {
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
