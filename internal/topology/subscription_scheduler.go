package topology

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/scanloop"
	"github.com/Resinat/Resin/internal/subscription"
)

const schedulerLookahead = 15 * time.Second

// UpstreamNodePicker resolves a "platform:<name>" upstream selector to a node
// hash. Implemented by routing.Router (PickLowestLatencyNode); declared here as
// an interface so topology does not import routing.
type UpstreamNodePicker interface {
	// PickLowestLatencyNodeExcluding picks the lowest-latency routable node in
	// the platform, never returning a node owned by excludeSubID (a
	// subscription must not use its own node as its upstream).
	PickLowestLatencyNodeExcluding(platformName string, excludeSubID string) (node.Hash, error)
}

// SubscriptionScheduler manages periodic subscription updates.
type SubscriptionScheduler struct {
	subManager     *SubscriptionManager
	pool           *GlobalNodePool
	downloader     netutil.Downloader
	downloadCtx    context.Context
	cancelDownload context.CancelFunc

	// Fetcher fetches subscription data from a URL with an optional
	// per-subscription User-Agent override (empty string = default UA).
	// Defaults to downloader.Download; injectable for testing.
	Fetcher func(url string, userAgent string) ([]byte, error)

	// For persistence.
	onSubUpdated func(sub *subscription.Subscription)
	// onSubReenabledNode is called for each non-evicted node hash when a
	// subscription transitions from disabled to enabled.
	onSubReenabledNode func(hash node.Hash)
	// onRegisterUpstream registers a subscription-level upstream proxy
	// outbound (URI mode) before pool diff.
	onRegisterUpstream func(subID string, upstreamURI string) error
	// onUnregisterUpstream unregisters a subscription-level upstream proxy
	// outbound (URI mode) after pool diff.
	onUnregisterUpstream func(subID string)
	// upstreamPicker resolves a "platform:<name>" upstream to a concrete node.
	// Nil disables platform-mode upstreams.
	upstreamPicker UpstreamNodePicker

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// SchedulerConfig configures the SubscriptionScheduler.
type SchedulerConfig struct {
	SubManager   *SubscriptionManager
	Pool         *GlobalNodePool
	Downloader   netutil.Downloader               // shared downloader
	Fetcher      func(url string, userAgent string) ([]byte, error) // optional, defaults to Downloader.Download
	OnSubUpdated func(sub *subscription.Subscription)
	// OnSubReenabledNode is fired after false->true enabled transition.
	OnSubReenabledNode func(hash node.Hash)
	// OnRegisterUpstream registers a subscription-level upstream proxy
	// outbound (URI mode) before pool diff. Idempotent.
	OnRegisterUpstream func(subID string, upstreamURI string) error
	// OnUnregisterUpstream unregisters a subscription-level upstream proxy
	// outbound (URI mode) after pool diff when the new upstream is no longer
	// a URI. No-op if nothing was registered.
	OnUnregisterUpstream func(subID string)
	// UpstreamNodePicker resolves "platform:<name>" upstreams to a node hash.
	// Optional; when nil, platform-mode upstreams fail at refresh time.
	UpstreamNodePicker UpstreamNodePicker
}

// NewSubscriptionScheduler creates a new scheduler.
func NewSubscriptionScheduler(cfg SchedulerConfig) *SubscriptionScheduler {
	downloadCtx, cancelDownload := context.WithCancel(context.Background())
	sched := &SubscriptionScheduler{
		subManager:         cfg.SubManager,
		pool:               cfg.Pool,
		downloader:         cfg.Downloader,
		downloadCtx:        downloadCtx,
		cancelDownload:     cancelDownload,
		onSubUpdated:       cfg.OnSubUpdated,
		onSubReenabledNode: cfg.OnSubReenabledNode,
		onRegisterUpstream: cfg.OnRegisterUpstream,
		onUnregisterUpstream: cfg.OnUnregisterUpstream,
		upstreamPicker:     cfg.UpstreamNodePicker,
		stopCh:             make(chan struct{}),
	}
	if cfg.Fetcher != nil {
		sched.Fetcher = cfg.Fetcher
	} else {
		sched.Fetcher = sched.fetchViaDownloader
	}
	return sched
}

// SetUpstreamNodePicker injects the platform-mode upstream resolver. Must be
// called before Start: the scheduler is constructed before the router.
func (s *SubscriptionScheduler) SetUpstreamNodePicker(p UpstreamNodePicker) {
	s.upstreamPicker = p
}

// Start launches the background scheduler goroutine.
func (s *SubscriptionScheduler) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		scanloop.Run(s.stopCh, scanloop.DefaultMinInterval, scanloop.DefaultJitterRange, s.tick)
	}()
}

// Stop signals the scheduler to stop and waits for it to finish.
func (s *SubscriptionScheduler) Stop() {
	close(s.stopCh)
	s.cancelDownload()
	s.wg.Wait()
}

// ForceRefreshAll unconditionally updates ALL enabled subscriptions, regardless
// of their next-check timestamps. Called once at startup to compensate for
// lost data from weak persistence (DESIGN.md step 8 batch 3).
// Updates run in parallel, and this method waits until all started updates exit.
func (s *SubscriptionScheduler) ForceRefreshAll() {
	select {
	case <-s.stopCh:
		return
	default:
	}

	subsToRefresh := make([]*subscription.Subscription, 0, s.subManager.Size())
	s.subManager.Range(func(id string, sub *subscription.Subscription) bool {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		if sub.Enabled() {
			subsToRefresh = append(subsToRefresh, sub)
		}
		return true
	})
	s.runUpdatesWithWorkerLimit(subsToRefresh)
}

// ForceRefreshAllAsync triggers ForceRefreshAll in a background goroutine.
// The goroutine is tracked by scheduler waitgroup so Stop() waits for exit.
func (s *SubscriptionScheduler) ForceRefreshAllAsync() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.ForceRefreshAll()
	}()
}

func (s *SubscriptionScheduler) tick() {
	select {
	case <-s.stopCh:
		return
	default:
	}

	now := time.Now().UnixNano()
	dueSubs := make([]*subscription.Subscription, 0, s.subManager.Size())
	s.subManager.Range(func(id string, sub *subscription.Subscription) bool {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		if !sub.Enabled() {
			return true
		}
		// Early retry requested after a transient platform-upstream failure.
		if retryAt := sub.UpstreamRetryAtNs(); retryAt > 0 {
			if now >= retryAt {
				sub.ClearUpstreamRetry()
				dueSubs = append(dueSubs, sub)
			}
			return true
		}
		// Re-pick when the resolved platform upstream node is no longer routable.
		if s.platformUpstreamStale(sub) {
			dueSubs = append(dueSubs, sub)
			return true
		}
		// Check if due: lastChecked + interval - lookahead <= now.
		if sub.LastCheckedNs.Load()+sub.UpdateIntervalNs()-int64(schedulerLookahead) <= now {
			dueSubs = append(dueSubs, sub)
		}
		return true
	})
	s.runUpdatesWithWorkerLimit(dueSubs)
}

// platformUpstreamStale reports whether the subscription uses a platform:<name>
// upstream whose previously resolved node has dropped out of that platform's
// routable view (circuit opened, subscription disabled, filters changed, node
// removed, ...). Such a subscription must be refreshed so a new upstream node
// is picked.
//
// The check is O(1) and runs once per tick, which bounds the switch latency to
// one scan interval (13-17s) without hooking into the circuit-breaker path.
func (s *SubscriptionScheduler) platformUpstreamStale(sub *subscription.Subscription) bool {
	if s.pool == nil || !subscription.IsUpstreamPlatform(sub.UpstreamProxy()) {
		return false
	}
	resolved := sub.UpstreamResolvedHash()
	if resolved == "" {
		// Not resolved yet: either never refreshed or a refresh is pending.
		// Let the normal interval handle it rather than looping here.
		return false
	}
	// Cooldown: a refresh triggered by this check already ran recently but the
	// staleness persists (e.g. the refresh failed for an unrelated reason such
	// as a download error). Back off instead of re-downloading every tick.
	if last := sub.LastCheckedNs.Load(); last > 0 &&
		time.Since(time.Unix(0, last)) < platformStaleRefreshCooldown {
		return false
	}
	name := subscription.ParsePlatformName(sub.UpstreamProxy())
	if name == "" {
		return false
	}
	plat, ok := s.pool.GetPlatformByName(name)
	if !ok {
		// Platform vanished; nothing to re-pick. Refresh will report the error.
		return false
	}
	hash, err := node.ParseHex(resolved)
	if err != nil {
		return false
	}
	return !plat.View().Contains(hash)
}

func (s *SubscriptionScheduler) runUpdatesWithWorkerLimit(subs []*subscription.Subscription) {
	if len(subs) == 0 {
		return
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(subs) {
		workers = len(subs)
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, sub := range subs {
		select {
		case <-s.stopCh:
			wg.Wait()
			return
		default:
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(sub *subscription.Subscription) {
			defer wg.Done()
			defer func() { <-sem }()
			select {
			case <-s.stopCh:
				return
			default:
			}
			s.UpdateSubscription(sub)
		}(sub)
	}
	wg.Wait()
}

// UpdateSubscription fetches and parses outside the lock, then diffs and
// applies the result under WithSubLock. This keeps the lock scope narrow
// (no I/O under lock) while still preventing concurrent diff/apply races.
func (s *SubscriptionScheduler) UpdateSubscription(sub *subscription.Subscription) {
	attemptStartedNs := time.Now().UnixNano()
	attemptSeq := sub.NextAttemptSeq()
	attemptURL := sub.URL()
	attemptSourceType := sub.SourceType()
	attemptUserAgent := sub.UserAgent()
	attemptContent := sub.Content()
	attemptConfigVersion := sub.ConfigVersion()
	attemptUpstream := sub.UpstreamProxy()

	// 1. Fetch/read content (lock-free).
	var (
		body []byte
		err  error
	)
	if attemptSourceType == subscription.SourceTypeLocal {
		body = []byte(attemptContent)
	} else {
		body, err = s.Fetcher(attemptURL, attemptUserAgent)
		if err != nil {
			s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "fetch", err)
			return
		}
	}

	// 2. Parse (lock-free).
	parsed, err := subscription.ParseGeneralSubscription(body)
	if err != nil {
		s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "parse", err)
		return
	}

	// 2b. Inject upstream detour into parsed node options (lock-free).
	// This changes each ParsedNode's RawOptions (adds detour field) so that
	// the node outbound will dial through the subscription's upstream proxy.
	// A "platform:<name>" upstream is resolved to a concrete node first and
	// then injected as "ref:<hash>", reusing the node-reference path.
	injectedUpstream := attemptUpstream
	platformResolved := false
	if subscription.IsUpstreamPlatform(attemptUpstream) {
		resolved, perr := s.resolvePlatformUpstream(sub, attemptUpstream)
		if perr != nil {
			s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "upstream_platform", perr)
			return
		}
		injectedUpstream = resolved
		platformResolved = true
	}
	parsed = subscription.ApplyUpstreamDetour(parsed, injectedUpstream, sub.ID)

	// 2b-2. Cycle check for ref-mode upstream: a "ref:<hash>" chain must
	// not form a cycle (only ref references can close a Resin-internal loop; URI
	// upstreams point outside Resin and cannot cycle).
	if subscription.IsUpstreamRef(injectedUpstream) && s.pool != nil {
		if err := subscription.ValidateUpstreamRefNoCycle(sub.ID, injectedUpstream, s.subManager, s.pool); err != nil {
			s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "upstream_cycle", err)
			return
		}
	}

	// 2c. Register URI-mode upstream outbound in sbOutbound manager
	// (must happen before pool diff so node outbounds can resolve the detour).
	isURIUpstream := attemptUpstream != "" && !subscription.IsUpstreamRef(attemptUpstream) && !subscription.IsUpstreamPlatform(attemptUpstream)
	if isURIUpstream && s.onRegisterUpstream != nil {
		if err := s.onRegisterUpstream(sub.ID, attemptUpstream); err != nil {
			s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "register_upstream", err)
			return
		}
	}

	// 3. Build refreshed managed nodes map (lock-free, pure computation).
	refreshedManagedNodes := subscription.NewManagedNodes()
	rawByHash := make(map[node.Hash][]byte)
	for _, p := range parsed {
		h := node.HashFromRawOptions(p.RawOptions)
		existing, _ := refreshedManagedNodes.LoadNode(h)
		existing.Tags = append(existing.Tags, p.Tag)
		refreshedManagedNodes.StoreNode(h, existing)
		if _, ok := rawByHash[h]; !ok {
			rawByHash[h] = p.RawOptions
		}
	}

	// 4. Diff, swap, add/remove — under lock.
	applied := false
	sub.WithOpLock(func() {
		// If refresh-input config changed while this attempt was in-flight, discard.
		if sub.ConfigVersion() != attemptConfigVersion {
			return
		}
		// Stale success guard: if a newer successful update has already landed,
		// discard this older attempt to avoid rolling state backward.
		if sub.LastAppliedSeq() > attemptSeq {
			return
		}

		old := sub.ManagedNodes()
		mergedManagedNodes := refreshedManagedNodes
		if sub.IncrementalAliveNodes() {
			mergedManagedNodes = subscription.NewManagedNodes()
			old.RangeNodes(func(h node.Hash, oldNode subscription.ManagedNode) bool {
				if oldNode.Evicted {
					mergedManagedNodes.StoreNode(h, oldNode)
					return true
				}
				if entry, ok := s.pool.GetEntry(h); ok && !shouldRemoveUnhealthyNodeForIncrementalMode(entry) {
					mergedManagedNodes.StoreNode(h, oldNode)
				}
				return true
			})
			refreshedManagedNodes.RangeNodes(func(h node.Hash, refreshedNode subscription.ManagedNode) bool {
				mergedManagedNodes.StoreNode(h, refreshedNode)
				return true
			})
		}

		// Keep hashes inherit historical eviction state so refresh will not
		// re-add previously evicted nodes back into pool.
		old.RangeNodes(func(h node.Hash, oldNode subscription.ManagedNode) bool {
			if !oldNode.Evicted {
				return true
			}
			nextNode, ok := mergedManagedNodes.LoadNode(h)
			if !ok {
				return true
			}
			nextNode.Evicted = true
			mergedManagedNodes.StoreNode(h, nextNode)
			return true
		})
		added, kept, removed := subscription.DiffHashes(old, mergedManagedNodes)

		sub.SwapManagedNodes(mergedManagedNodes)

		for _, h := range added {
			managed, ok := mergedManagedNodes.LoadNode(h)
			if ok && managed.Evicted {
				continue
			}
			raw := rawByHash[h]
			if len(raw) == 0 {
				if entry, ok := s.pool.GetEntry(h); ok {
					raw = entry.RawOptions
				}
			}
			if len(raw) == 0 {
				continue
			}
			s.pool.AddNodeFromSub(h, raw, sub.ID)
		}
		for _, h := range kept {
			managed, ok := mergedManagedNodes.LoadNode(h)
			if ok && managed.Evicted {
				continue
			}
			raw := rawByHash[h]
			if len(raw) == 0 {
				if entry, ok := s.pool.GetEntry(h); ok {
					raw = entry.RawOptions
				}
			}
			if len(raw) == 0 {
				continue
			}
			s.pool.AddNodeFromSub(h, raw, sub.ID)
		}
		for _, h := range removed {
			s.pool.RemoveNodeFromSub(h, sub.ID)
		}
		// 5. Update timestamps (inside lock, using current time).
		now := time.Now().UnixNano()
		sub.LastCheckedNs.Store(now)
		sub.LastUpdatedNs.Store(now)
		// Record which node a platform upstream resolved to so the scheduler
		// can detect later that the picked node left the platform's routable
		// view. Written under the op lock so a concurrent upstream change (which
		// bumps configVersion and clears this field) cannot be overwritten by a
		// stale attempt.
		if platformResolved {
			sub.SetUpstreamResolvedHash(subscription.ParseUpstreamRefHash(injectedUpstream))
		} else {
			sub.SetUpstreamResolvedHash("")
		}
		sub.ClearUpstreamRetry()
		sub.MarkAppliedAttempt(attemptSeq)
		sub.SetLastError("")
		applied = true
	})
	if !applied {
		log.Printf("[scheduler] stale success ignored for %s", sub.ID)
		return
	}

	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}

	// After successful apply: if the new upstream is no longer a URI
	// (empty or ref), unregister any previously registered URI upstream.
	if !isURIUpstream && s.onUnregisterUpstream != nil {
		s.onUnregisterUpstream(sub.ID)
	}
}

// resolvePlatformUpstream resolves a "platform:<name>" upstream selector to a
// concrete "ref:<hash>" value using the platform's routable node set.
//
// Latency policy is fixed to lowest-latency (the operator opted for this when
// choosing platform mode). Permanent configuration errors (unknown platform,
// malformed selector) fail the refresh and wait for the normal interval;
// transient ones (no routable node yet, e.g. during startup before any
// subscription populated the pool) additionally schedule a short retry so the
// subscription recovers without waiting hours.
func (s *SubscriptionScheduler) resolvePlatformUpstream(
	sub *subscription.Subscription,
	upstream string,
) (string, error) {
	name := subscription.ParsePlatformName(upstream)
	if name == "" {
		return "", fmt.Errorf("upstream_proxy: invalid platform selector %q", upstream)
	}
	if s.upstreamPicker == nil {
		return "", fmt.Errorf("upstream_proxy: platform %q is not supported in this build", name)
	}
	hash, err := s.upstreamPicker.PickLowestLatencyNodeExcluding(name, sub.ID)
	if err != nil {
		if errors.Is(err, routing.ErrNoAvailableNodes) {
			sub.ScheduleUpstreamRetry(time.Now().Add(upstreamRetryDelay).UnixNano())
			return "", fmt.Errorf("upstream_proxy: platform %q has no routable node yet: %v", name, err)
		}
		return "", fmt.Errorf("upstream_proxy: platform %q: %v", name, err)
	}
	return "ref:" + hash.Hex(), nil
}

// upstreamRetryDelay is how soon a subscription whose platform upstream has no
// routable node yet should be retried.
const upstreamRetryDelay = 30 * time.Second

// platformStaleRefreshCooldown bounds how often a stale platform upstream can
// force a refresh, so a persistently failing refresh cannot hammer the source.
const platformStaleRefreshCooldown = 60 * time.Second

// handleUpdateFailure applies a fetch/parse failure to subscription state.
// It ignores stale failures from an outdated attempt (config-version guard +
// LastUpdatedNs stale-success guard).

func shouldRemoveUnhealthyNodeForIncrementalMode(entry *node.NodeEntry) bool {
	if entry == nil {
		return false
	}
	return entry.IsCircuitOpen() || (!entry.HasOutbound() && entry.GetLastError() != "")
}

func (s *SubscriptionScheduler) handleUpdateFailure(
	sub *subscription.Subscription,
	attemptStartedNs int64,
	attemptSeq int64,
	attemptConfigVersion int64,
	stage string,
	err error,
) {
	applied := false
	sub.WithOpLock(func() {
		// If refresh-input config changed while this attempt was in-flight, discard.
		if sub.ConfigVersion() != attemptConfigVersion {
			return
		}
		if sub.LastAppliedSeq() > attemptSeq {
			return
		}
		now := time.Now().UnixNano()
		sub.LastCheckedNs.Store(now)
		sub.SetLastError(err.Error())
		applied = true
	})
	if !applied {
		log.Printf("[scheduler] stale %s failure ignored for %s: %v", stage, sub.ID, err)
		return
	}

	log.Printf("[scheduler] %s %s failed: %v", stage, sub.ID, err)
	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}
}

// SetSubscriptionEnabled updates the enabled flag and rebuilds all platform
// routable views. Disabling a subscription makes its nodes invisible to
// platform tag-regex matching; enabling makes them visible again.
func (s *SubscriptionScheduler) SetSubscriptionEnabled(sub *subscription.Subscription, enabled bool) {
	wasEnabled := false
	var candidateHashes []node.Hash
	wasDisabled := make(map[node.Hash]struct{})
	sub.WithOpLock(func() {
		wasEnabled = sub.Enabled()

		if !wasEnabled && enabled {
			sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
				if managed.Evicted {
					return true
				}
				candidateHashes = append(candidateHashes, h)
				if s.pool != nil && s.pool.IsNodeDisabled(h) {
					wasDisabled[h] = struct{}{}
				}
				return true
			})
		}

		sub.SetEnabled(enabled)
	})
	// Rebuild all platform views so they pick up the visibility change.
	s.pool.RebuildAllPlatforms()

	// On re-enable, immediately re-check node outbound/probe state for nodes
	// that actually transitioned from disabled -> enabled.
	if !wasEnabled && enabled && s.onSubReenabledNode != nil {
		for _, h := range candidateHashes {
			if _, ok := wasDisabled[h]; !ok {
				continue
			}
			if s.pool.IsNodeDisabled(h) {
				continue
			}
			s.onSubReenabledNode(h)
		}
	}

	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}
}

// RenameSubscription updates the subscription name and re-triggers platform
// re-evaluation for all managed nodes (since tags include the sub name).
func (s *SubscriptionScheduler) RenameSubscription(sub *subscription.Subscription, newName string) {
	sub.WithOpLock(func() {
		sub.SetName(newName)
		// Re-add all managed hashes to trigger platform re-filter.
		sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
			if managed.Evicted {
				return true
			}
			entry, ok := s.pool.GetEntry(h)
			if ok {
				s.pool.AddNodeFromSub(h, entry.RawOptions, sub.ID)
			}
			return true
		})
	})
}

func (s *SubscriptionScheduler) fetchViaDownloader(url string, userAgent string) ([]byte, error) {
	if userAgent != "" {
		if uaDownloader, ok := s.downloader.(netutil.UserAgentDownloader); ok && uaDownloader != nil {
			return uaDownloader.DownloadWithUserAgent(s.downloadCtx, url, userAgent)
		}
	}
	return s.downloader.Download(s.downloadCtx, url)
}
