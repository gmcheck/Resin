package routing

import (
	"bytes"
	"log"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

// PickLowestLatencyNode selects the routable node with the lowest recent
// authority-domain latency in the named platform. It is used to resolve a
// subscription-level "platform:<name>" upstream proxy into a concrete node.
//
// Selection rules:
//   - Only nodes in Platform.View are considered; the view already excludes
//     disabled, unhealthy (outbound not ready / circuit open), tag- and
//     region-filtered, egress-IP-less and latency-less nodes.
//   - Latency is the recent authority-domain average (same helper and window
//     as randomRoute), so stale samples are ignored.
//   - Nodes without a recent authority sample are skipped. If no node has one,
//     the pick falls back to the lowest hash and logs why, so that a fresh pool
//     that has not been probed yet does not block subscription refresh.
//
// The pick is exact (full scan) rather than P2C: it runs once per refresh, not
// on the data path.
func (r *Router) PickLowestLatencyNode(platName string) (node.Hash, error) {
	return r.PickLowestLatencyNodeExcluding(platName, "")
}

// PickLowestLatencyNodeExcluding is PickLowestLatencyNode but never returns a
// node owned by excludeSubID. A subscription using platform mode must not pick
// one of its own nodes as its upstream: every node would then dial through
// itself (a one-hop cycle).
func (r *Router) PickLowestLatencyNodeExcluding(platName string, excludeSubID string) (node.Hash, error) {
	plat, err := r.resolvePlatform(platName)
	if err != nil {
		return node.Zero, err
	}
	return r.pickLowestLatencyNode(plat, excludeSubID)
}

func (r *Router) pickLowestLatencyNode(plat *platform.Platform, excludeSubID string) (node.Hash, error) {
	view := plat.View()
	if view.Size() == 0 {
		return node.Zero, ErrNoAvailableNodes
	}

	now := time.Now()
	var authorities []string
	if r.authorities != nil {
		authorities = r.authorities()
	}
	window := 10 * time.Minute
	if r.p2cWindow != nil {
		window = r.p2cWindow()
	}

	bestHash := node.Zero
	var bestLatency time.Duration
	fallbackHash := node.Zero

	view.Range(func(h node.Hash) bool {
		entry, ok := r.pool.GetEntry(h)
		if !ok || entry == nil {
			return true
		}
		if !entry.IsHealthy() {
			return true
		}
		if excludeSubID != "" && entryHasSubscription(entry, excludeSubID) {
			return true
		}
		if fallbackHash == node.Zero || hashLess(h, fallbackHash) {
			fallbackHash = h
		}
		latency, hasLatency := averageRecentAuthorityLatency(entry, authorities, now, window)
		if !hasLatency {
			return true
		}
		if bestHash == node.Zero || latency < bestLatency || (latency == bestLatency && hashLess(h, bestHash)) {
			bestHash = h
			bestLatency = latency
		}
		return true
	})

	if bestHash != node.Zero {
		return bestHash, nil
	}
	if fallbackHash == node.Zero {
		return node.Zero, ErrNoAvailableNodes
	}
	log.Printf("[upstream] platform %q: no recent authority latency for any candidate, falling back to %s", plat.Name, fallbackHash.Hex())
	return fallbackHash, nil
}

// hashLess reports whether a sorts before b. Used as a deterministic tie-break
// so repeated picks return the same node when latencies are equal.
func hashLess(a, b node.Hash) bool {
	return bytes.Compare(a[:], b[:]) < 0
}

// entryHasSubscription reports whether the node belongs to the given
// subscription.
func entryHasSubscription(entry *node.NodeEntry, subID string) bool {
	for _, id := range entry.SubscriptionIDs() {
		if id == subID {
			return true
		}
	}
	return false
}
