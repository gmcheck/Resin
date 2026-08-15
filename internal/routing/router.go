package routing

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/netip"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

var (
	ErrPlatformNotFound = errors.New("platform not found")
)

type PoolAccessor interface {
	GetEntry(hash node.Hash) (*node.NodeEntry, bool)
	GetPlatform(id string) (*platform.Platform, bool)
	GetPlatformByName(name string) (*platform.Platform, bool)
	RangePlatforms(fn func(*platform.Platform) bool)
}

// Router handles route selection and lease management.
type Router struct {
	pool            PoolAccessor
	states          *xsync.Map[string, *PlatformRoutingState]
	authorities     func() []string
	p2cWindow       func() time.Duration
	onLeaseEvent    LeaseEventFunc
	nodeTagResolver func(node.Hash) string
}

type RouterConfig struct {
	Pool        PoolAccessor
	Authorities func() []string
	P2CWindow   func() time.Duration
	// OnLeaseEvent is called synchronously; handlers must stay lightweight.
	OnLeaseEvent LeaseEventFunc
	// NodeTagResolver resolves a node hash to its display tag ("<Sub>/<Tag>").
	// If nil, NodeTag will be empty.
	NodeTagResolver func(node.Hash) string
}

func NewRouter(cfg RouterConfig) *Router {
	return &Router{
		pool:            cfg.Pool,
		states:          xsync.NewMap[string, *PlatformRoutingState](),
		authorities:     cfg.Authorities,
		p2cWindow:       cfg.P2CWindow,
		onLeaseEvent:    cfg.OnLeaseEvent,
		nodeTagResolver: cfg.NodeTagResolver,
	}
}

type RouteResult struct {
	PlatformID   string
	PlatformName string
	NodeHash     node.Hash
	EgressIP     netip.Addr
	NodeTag      string // display tag: "<Subscription>/<Tag>" (DESIGN.md §601)
	LeaseCreated bool
}

const livePickAttempts = 2 // first pick + one retry

type leaseInvalidationReason int

const (
	leaseInvalidationNone leaseInvalidationReason = iota
	leaseInvalidationExpire
	leaseInvalidationRemove
)

func (r *Router) RouteRequest(platName, account, target string) (RouteResult, error) {
	plat, err := r.resolvePlatform(platName)
	if err != nil {
		return RouteResult{}, err
	}

	targetDomain := netutil.ExtractDomain(target)
	state := r.ensurePlatformState(plat.ID)
	var result RouteResult
	if account == "" {
		result, err = r.routeRandom(plat, state, targetDomain)
	} else {
		result, err = r.routeSticky(plat, state, account, targetDomain, time.Now())
	}
	if err != nil {
		return RouteResult{}, err
	}
	result = withPlatformContext(plat, result)
	if r.nodeTagResolver != nil {
		result.NodeTag = r.nodeTagResolver(result.NodeHash)
	}
	return result, nil
}

func withPlatformContext(plat *platform.Platform, res RouteResult) RouteResult {
	res.PlatformID = plat.ID
	res.PlatformName = plat.Name
	return res
}

func (r *Router) resolvePlatform(platName string) (*platform.Platform, error) {
	if platName == "" {
		if p, ok := r.pool.GetPlatform(platform.DefaultPlatformID); ok {
			return p, nil
		}
		return nil, ErrPlatformNotFound
	}
	p, ok := r.pool.GetPlatformByName(platName)
	if !ok {
		return nil, ErrPlatformNotFound
	}
	return p, nil
}

func (r *Router) ensurePlatformState(platformID string) *PlatformRoutingState {
	state, _ := r.states.LoadOrCompute(platformID, func() (*PlatformRoutingState, bool) {
		return NewPlatformRoutingState(), false
	})
	return state
}

func (r *Router) routeRandom(
	plat *platform.Platform,
	state *PlatformRoutingState,
	targetDomain string,
) (RouteResult, error) {
	h, entry, err := r.selectLiveRandomRoute(plat, state.IPLoadStats, targetDomain)
	if err != nil {
		return RouteResult{}, err
	}
	return RouteResult{
		NodeHash:     h,
		EgressIP:     entry.GetEgressIP(),
		LeaseCreated: false,
	}, nil
}

func (r *Router) routeSticky(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
) (RouteResult, error) {
	nowNs := now.UnixNano()
	var result RouteResult
	var routeErr error

	_, _ = state.Leases.leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		newLease, op, routeResult, err := r.decideStickyLease(
			plat,
			state,
			account,
			targetDomain,
			now,
			nowNs,
			current,
			loaded,
		)
		if err != nil {
			routeErr = err
			return newLease, op
		}
		result = routeResult
		return newLease, op
	})

	return result, routeErr
}

func (r *Router) decideStickyLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
	nowNs int64,
	current Lease,
	loaded bool,
) (Lease, xsync.ComputeOp, RouteResult, error) {
	hadPreviousLease := loaded
	invalidation := leaseInvalidationNone

	if loaded && current.IsExpired(now) {
		invalidation = leaseInvalidationExpire
		loaded = false
	}

	if loaded {
		if newLease, hitResult, ok := r.tryLeaseHit(plat, account, current, nowNs); ok {
			return newLease, xsync.UpdateOp, hitResult, nil
		}
		if newLease, rotatedResult, ok := r.tryLeaseSameIPRotation(plat, account, current, targetDomain, nowNs); ok {
			return newLease, xsync.UpdateOp, rotatedResult, nil
		}
		invalidation = leaseInvalidationRemove
	}

	return r.createOrAbortStickyLease(
		plat,
		state,
		account,
		targetDomain,
		now,
		nowNs,
		current,
		hadPreviousLease,
		invalidation,
	)
}

func (r *Router) createOrAbortStickyLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
	nowNs int64,
	previous Lease,
	hadPreviousLease bool,
	invalidation leaseInvalidationReason,
) (Lease, xsync.ComputeOp, RouteResult, error) {
	newLease, createdResult, err := r.createLease(plat, state, account, targetDomain, now, nowNs)
	if err != nil {
		r.cleanupPreviousLease(state, previous, hadPreviousLease, invalidation, plat.ID, account)
		lease, op := abortLeaseCreate(previous, hadPreviousLease)
		return lease, op, RouteResult{}, err
	}

	r.cleanupPreviousLease(state, previous, hadPreviousLease, invalidation, plat.ID, account)
	state.IPLoadStats.Inc(newLease.EgressIP)
	r.emitLeaseEvent(LeaseEvent{
		Type:       LeaseCreate,
		PlatformID: plat.ID,
		Account:    account,
		NodeHash:   newLease.NodeHash,
		EgressIP:   newLease.EgressIP,
	})
	return newLease, xsync.UpdateOp, createdResult, nil
}

func (r *Router) tryLeaseHit(
	plat *platform.Platform,
	account string,
	current Lease,
	nowNs int64,
) (Lease, RouteResult, bool) {
	entry, ok := r.pool.GetEntry(current.NodeHash)
	if !ok || !plat.View().Contains(current.NodeHash) || entry.GetEgressIP() != current.EgressIP {
		return Lease{}, RouteResult{}, false
	}

	newLease := current
	newLease.LastAccessedNs = nowNs
	r.emitLeaseEvent(LeaseEvent{
		Type:       LeaseTouch,
		PlatformID: plat.ID,
		Account:    account,
		NodeHash:   current.NodeHash,
		EgressIP:   current.EgressIP,
	})
	return newLease, RouteResult{
		NodeHash:     current.NodeHash,
		EgressIP:     current.EgressIP,
		LeaseCreated: false,
	}, true
}

func (r *Router) tryLeaseSameIPRotation(
	plat *platform.Platform,
	account string,
	current Lease,
	targetDomain string,
	nowNs int64,
) (Lease, RouteResult, bool) {
	bestHash, ok := chooseSameIPRotationCandidate(
		plat,
		r.pool,
		current.EgressIP,
		targetDomain,
		r.authorities(),
		r.p2cWindow(),
	)
	if !ok {
		return Lease{}, RouteResult{}, false
	}

	newLease := current
	newLease.NodeHash = bestHash
	newLease.LastAccessedNs = nowNs
	r.emitLeaseEvent(LeaseEvent{
		Type:       LeaseReplace,
		PlatformID: plat.ID,
		Account:    account,
		NodeHash:   bestHash,
		EgressIP:   current.EgressIP,
	})
	return newLease, RouteResult{
		NodeHash:     bestHash,
		EgressIP:     current.EgressIP,
		LeaseCreated: false,
	}, true
}

func (r *Router) createLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	now time.Time,
	nowNs int64,
) (Lease, RouteResult, error) {
	h, entry, err := r.selectRouteForLease(plat, state, account, targetDomain, nowNs)
	if err != nil {
		return Lease{}, RouteResult{}, err
	}
	ttl := plat.StickyTTLNs
	if ttl <= 0 {
		ttl = int64(24 * time.Hour) // Default safeguard
	}

	lease := Lease{
		NodeHash:       h,
		EgressIP:       entry.GetEgressIP(),
		CreatedAtNs:    nowNs,
		ExpiryNs:       now.Add(time.Duration(ttl)).UnixNano(),
		LastAccessedNs: nowNs,
	}
	return lease, RouteResult{
		NodeHash:     lease.NodeHash,
		EgressIP:     lease.EgressIP,
		LeaseCreated: true,
	}, nil
}

// quotaPickAttempts bounds pick-retries when the per-IP account quota blocks
// the randomly selected node (design §4).
const quotaPickAttempts = 8

// selectRouteForLease picks a node for a new sticky lease of account. When the
// platform has no per-IP account quota it delegates to the plain live-random
// path; otherwise every pick is admitted through IPAccountWindow.ReserveIfEligible,
// which atomically checks the quota and records the account→IP binding.
func (r *Router) selectRouteForLease(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	targetDomain string,
	nowNs int64,
) (node.Hash, *node.NodeEntry, error) {
	if !plat.IPQuotaEnabled() {
		return r.selectLiveRandomRoute(plat, state.IPLoadStats, targetDomain)
	}

	windowNs := plat.EffectiveIPAccountWindowNs()
	max := plat.MaxAccountsPerIP
	win := state.IPWindow
	for i := 0; i < quotaPickAttempts; i++ {
		h, entry, err := r.pickLiveRandomRoute(plat, state.IPLoadStats, targetDomain)
		if err != nil {
			return node.Zero, nil, err
		}
		if entry == nil {
			continue // picked node vanished from pool; retry
		}
		if win.ReserveIfEligible(entry.GetEgressIP(), account, nowNs, windowNs, max) {
			return h, entry, nil
		}
	}
	return r.fallbackQuotaRoute(plat, state, account, nowNs, windowNs, max)
}

// fallbackQuotaRoute is the fail-open fallback for when every pick was blocked
// by the per-IP account quota: it selects the routable IP with the fewest
// distinct in-window accounts (tie: lowest live lease load) and force-reserves
// it, subject to a hard ceiling of max+1 accounts per IP per window. If no IP
// is under the ceiling, the request fails closed with ErrNoAvailableNodes.
func (r *Router) fallbackQuotaRoute(
	plat *platform.Platform,
	state *PlatformRoutingState,
	account string,
	nowNs int64,
	windowNs int64,
	max int,
) (node.Hash, *node.NodeEntry, error) {
	counts := state.IPWindow.CountsByIP(nowNs, windowNs)

	type ipCandidate struct {
		ip    netip.Addr
		hash  node.Hash
		entry *node.NodeEntry
		count int
		load  int64
	}
	seen := make(map[netip.Addr]bool)
	var best *ipCandidate
	plat.View().Range(func(h node.Hash) bool {
		entry, ok := r.pool.GetEntry(h)
		if !ok {
			return true
		}
		ip := entry.GetEgressIP()
		if !ip.IsValid() || seen[ip] {
			return true
		}
		seen[ip] = true
		cand := ipCandidate{
			ip:    ip,
			hash:  h,
			entry: entry,
			count: counts[ip],
			load:  state.IPLoadStats.Get(ip),
		}
		if best == nil || cand.count < best.count || (cand.count == best.count && cand.load < best.load) {
			copied := cand
			best = &copied
		}
		return true
	})
	if best == nil {
		return node.Zero, nil, ErrNoAvailableNodes
	}

	if !state.IPWindow.ReserveForcedFallback(best.ip, account, nowNs, windowNs, max) {
		log.Printf(
			"event=ip_quota_exhausted platform=%s account=%s max=%d ceiling=%d",
			plat.ID, account, max, max+1,
		)
		return node.Zero, nil, ErrNoAvailableNodes
	}

	log.Printf(
		"event=ip_quota_exceeded_fallback platform=%s account=%s ip=%s window_accounts=%d max=%d lease_load=%d",
		plat.ID, account, best.ip, best.count, max, best.load,
	)
	return best.hash, best.entry, nil
}

func (r *Router) cleanupPreviousLease(
	state *PlatformRoutingState,
	lease Lease,
	hadPreviousLease bool,
	invalidation leaseInvalidationReason,
	platformID string,
	account string,
) {
	if !hadPreviousLease {
		return
	}
	state.Leases.stats.Dec(lease.EgressIP)
	switch invalidation {
	case leaseInvalidationExpire:
		r.emitLeaseEvent(LeaseEvent{
			Type:        LeaseExpire,
			PlatformID:  platformID,
			Account:     account,
			NodeHash:    lease.NodeHash,
			EgressIP:    lease.EgressIP,
			CreatedAtNs: lease.CreatedAtNs,
		})
	case leaseInvalidationRemove:
		r.emitLeaseEvent(LeaseEvent{
			Type:        LeaseRemove,
			PlatformID:  platformID,
			Account:     account,
			NodeHash:    lease.NodeHash,
			EgressIP:    lease.EgressIP,
			CreatedAtNs: lease.CreatedAtNs,
		})
	}
}

func abortLeaseCreate(current Lease, hadPreviousLease bool) (Lease, xsync.ComputeOp) {
	if hadPreviousLease {
		return current, xsync.DeleteOp
	}
	return current, xsync.CancelOp
}

func (r *Router) emitLeaseEvent(event LeaseEvent) {
	if r.onLeaseEvent != nil {
		r.onLeaseEvent(event)
	}
}

func (r *Router) selectLiveRandomRoute(
	plat *platform.Platform,
	stats *IPLoadStats,
	targetDomain string,
) (node.Hash, *node.NodeEntry, error) {
	var lastMissing node.Hash
	for i := 0; i < livePickAttempts; i++ {
		h, entry, err := r.pickLiveRandomRoute(plat, stats, targetDomain)
		if err != nil {
			return node.Zero, nil, err
		}
		if entry != nil {
			return h, entry, nil
		}
		lastMissing = h
	}
	if lastMissing != node.Zero {
		return node.Zero, nil, fmt.Errorf("%w: selected node %s no longer in pool", ErrNoAvailableNodes, lastMissing.Hex())
	}
	return node.Zero, nil, ErrNoAvailableNodes
}

// pickLiveRandomRoute performs one randomRoute pick and resolves the picked
// node to a live pool entry. It returns a nil entry (without error) when the
// picked node vanished from the pool between selection and lookup.
func (r *Router) pickLiveRandomRoute(
	plat *platform.Platform,
	stats *IPLoadStats,
	targetDomain string,
) (node.Hash, *node.NodeEntry, error) {
	h, err := randomRoute(plat, stats, r.pool, targetDomain, r.authorities(), r.p2cWindow())
	if err != nil {
		return node.Zero, nil, err
	}
	entry, ok := r.pool.GetEntry(h)
	if !ok {
		return h, nil, nil
	}
	return h, entry, nil
}

func chooseSameIPRotationCandidate(
	plat *platform.Platform,
	pool PoolAccessor,
	targetIP netip.Addr,
	targetDomain string,
	authorities []string,
	window time.Duration,
) (node.Hash, bool) {
	bestKnownHash := node.Zero
	bestKnownLatency := time.Duration(math.MaxInt64)
	fallbackHash := node.Zero

	plat.View().Range(func(h node.Hash) bool {
		entry, ok := pool.GetEntry(h)
		if !ok || entry.GetEgressIP() != targetIP {
			return true
		}
		if fallbackHash == node.Zero {
			fallbackHash = h
		}

		latency, hasLatency := sameIPCandidateLatency(entry, targetDomain, authorities, window)
		if hasLatency && latency < bestKnownLatency {
			bestKnownLatency = latency
			bestKnownHash = h
		}
		return true
	})

	if bestKnownHash != node.Zero {
		return bestKnownHash, true
	}
	if fallbackHash != node.Zero {
		return fallbackHash, true
	}
	return node.Zero, false
}

func sameIPCandidateLatency(
	entry *node.NodeEntry,
	targetDomain string,
	authorities []string,
	window time.Duration,
) (time.Duration, bool) {
	now := time.Now()
	if latency, ok := lookupRecentDomainLatency(entry, targetDomain, now, window); ok {
		return latency, true
	}

	if latency, ok := averageRecentAuthorityLatency(entry, authorities, now, window); ok {
		return latency, true
	}
	return 0, false
}

// ReadLease implements weak persistence read.
func (r *Router) ReadLease(key model.LeaseKey) *model.Lease {
	state, ok := r.states.Load(key.PlatformID)
	if !ok {
		return nil
	}
	lease, ok := state.Leases.GetLease(key.Account)
	if !ok {
		return nil
	}
	return &model.Lease{
		PlatformID:     key.PlatformID,
		Account:        key.Account,
		NodeHash:       lease.NodeHash.Hex(),
		EgressIP:       lease.EgressIP.String(),
		CreatedAtNs:    lease.CreatedAtNs,
		ExpiryNs:       lease.ExpiryNs,
		LastAccessedNs: lease.LastAccessedNs,
	}
}

// UpsertLease writes or replaces a lease for (platform_id, account).
// It updates per-IP lease counters and emits LeaseCreate/LeaseReplace events.
func (r *Router) UpsertLease(ml model.Lease) error {
	platformID := strings.TrimSpace(ml.PlatformID)
	if platformID == "" {
		return errors.New("platform_id is required")
	}
	account := strings.TrimSpace(ml.Account)
	if account == "" {
		return errors.New("account is required")
	}

	h, err := node.ParseHex(ml.NodeHash)
	if err != nil {
		return fmt.Errorf("parse node_hash: %w", err)
	}
	ip, err := netip.ParseAddr(ml.EgressIP)
	if err != nil {
		return fmt.Errorf("parse egress_ip: %w", err)
	}

	state := r.ensurePlatformState(platformID)
	lease := Lease{
		NodeHash:       h,
		EgressIP:       ip,
		CreatedAtNs:    ml.CreatedAtNs,
		ExpiryNs:       ml.ExpiryNs,
		LastAccessedNs: ml.LastAccessedNs,
	}

	eventType := LeaseCreate
	_, _ = state.Leases.leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if loaded {
			state.Leases.stats.Dec(current.EgressIP)
			eventType = LeaseReplace
		}
		state.Leases.stats.Inc(lease.EgressIP)
		return lease, xsync.UpdateOp
	})

	// Control-plane-created bindings also consume per-IP quota accounting:
	// risk control counts real distinct accounts per IP, not just hot-path ones.
	if plat, ok := r.pool.GetPlatform(platformID); ok && plat.IPQuotaEnabled() {
		state.IPWindow.Touch(ip, account, time.Now().UnixNano())
	}

	r.emitLeaseEvent(LeaseEvent{
		Type:       eventType,
		PlatformID: platformID,
		Account:    account,
		NodeHash:   lease.NodeHash,
		EgressIP:   lease.EgressIP,
	})
	return nil
}

// SnapshotIPLoad returns a best-effort point-in-time IP load snapshot for a platform.
// If the platform has no routing state yet, it returns an empty snapshot.
func (r *Router) SnapshotIPLoad(platformID string) map[netip.Addr]int64 {
	state, ok := r.states.Load(platformID)
	if !ok {
		return map[netip.Addr]int64{}
	}
	return state.IPLoadStats.Snapshot()
}

// RestoreLeases restores leases from persistence during bootstrap.
func (r *Router) RestoreLeases(leases []model.Lease) {
	nowNs := time.Now().UnixNano()
	for _, ml := range leases {
		h, err := node.ParseHex(ml.NodeHash)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(ml.EgressIP)
		if err != nil {
			continue
		}

		state, _ := r.states.LoadOrCompute(ml.PlatformID, func() (*PlatformRoutingState, bool) {
			return NewPlatformRoutingState(), false
		})

		l := Lease{
			NodeHash:       h,
			EgressIP:       ip,
			CreatedAtNs:    ml.CreatedAtNs,
			ExpiryNs:       ml.ExpiryNs,
			LastAccessedNs: ml.LastAccessedNs,
		}
		// Directly insert into table and stats
		state.Leases.CreateLease(ml.Account, l)

		// Seed the per-IP account window from restored leases so quota
		// accounting survives restarts: without this, an IP with restored
		// leases could immediately accept max fresh accounts on top.
		if plat, ok := r.pool.GetPlatform(ml.PlatformID); ok && plat.IPQuotaEnabled() {
			windowNs := plat.EffectiveIPAccountWindowNs()
			if nowNs-ml.CreatedAtNs < windowNs {
				state.IPWindow.Touch(ip, ml.Account, ml.CreatedAtNs)
			}
		}
	}
}

// SnapshotIPQuota returns a point-in-time view of the per-IP account quota
// state for observability. windowNs comes from the platform's effective
// configuration. Returns nil when the platform has no routing state.
func (r *Router) SnapshotIPQuota(platformID string, windowNs int64) *IPQuotaSnapshot {
	state, ok := r.states.Load(platformID)
	if !ok {
		return nil
	}
	nowNs := time.Now().UnixNano()
	blocked, fallback := state.IPWindow.Stats()
	return &IPQuotaSnapshot{
		BlockedTotal:  blocked,
		FallbackTotal: fallback,
		AccountsByIP:  state.IPWindow.CountsByIP(nowNs, windowNs),
	}
}

// IPQuotaSnapshot is a point-in-time view of per-IP account quota state.
type IPQuotaSnapshot struct {
	// BlockedTotal counts quota-blocked picks on the normal path.
	BlockedTotal int64
	// FallbackTotal counts forced fail-open reservations.
	FallbackTotal int64
	// AccountsByIP maps egress IP to distinct in-window account count.
	AccountsByIP map[netip.Addr]int
}

// RangeLeases iterates over all leases for a platform.
// Returns false if the platform has no routing state.
func (r *Router) RangeLeases(platformID string, fn func(account string, lease Lease) bool) bool {
	state, ok := r.states.Load(platformID)
	if !ok {
		return false
	}
	state.Leases.Range(fn)
	return true
}

// DeleteLease removes a single lease by platform and account.
// Returns true if a lease was deleted. Emits a LeaseRemove event.
func (r *Router) DeleteLease(platformID, account string) bool {
	state, ok := r.states.Load(platformID)
	if !ok {
		return false
	}
	lease, deleted := state.Leases.DeleteLease(account)
	if !deleted {
		return false
	}
	r.emitLeaseEvent(LeaseEvent{
		Type:        LeaseRemove,
		PlatformID:  platformID,
		Account:     account,
		NodeHash:    lease.NodeHash,
		EgressIP:    lease.EgressIP,
		CreatedAtNs: lease.CreatedAtNs,
	})
	return true
}

// DeleteAllLeases removes all leases for a platform.
// Returns the number of leases deleted. Emits a LeaseRemove event for each.
func (r *Router) DeleteAllLeases(platformID string) int {
	state, ok := r.states.Load(platformID)
	if !ok {
		return 0
	}
	count := 0
	state.Leases.Range(func(account string, _ Lease) bool {
		removed, deleted := state.Leases.DeleteLease(account)
		if deleted {
			r.emitLeaseEvent(LeaseEvent{
				Type:        LeaseRemove,
				PlatformID:  platformID,
				Account:     account,
				NodeHash:    removed.NodeHash,
				EgressIP:    removed.EgressIP,
				CreatedAtNs: removed.CreatedAtNs,
			})
			count++
		}
		return true
	})
	return count
}
