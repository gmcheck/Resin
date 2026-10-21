package subscription

import (
	"fmt"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

const maxUpstreamRefDepth = 10

// SubscriptionLookupForCycle provides read-only access to subscription instances
// for cycle detection. Implemented by topology.SubscriptionManager.
type SubscriptionLookupForCycle interface {
	// Get retrieves a subscription by ID.
	Get(id string) (*Subscription, bool)
}

// NodeLookupForCycle provides read-only access to node entries for cycle
// detection. Implemented by topology.GlobalNodePool.
type NodeLookupForCycle interface {
	// GetEntry returns the node entry for the given hash.
	GetEntry(hash node.Hash) (*node.NodeEntry, bool)
}

// PlatformViewForCycle is the minimal platform surface needed to expand a
// platform:<name> upstream edge: the set of currently routable node hashes.
// *platform.Platform satisfies it.
type PlatformViewForCycle interface {
	View() platform.ReadOnlyView
}

// PlatformLookupForCycle provides read-only access to platforms so that
// platform:<name> upstream edges can be expanded during cycle detection.
// Implemented by topology.GlobalNodePool; optional.
type PlatformLookupForCycle interface {
	GetPlatformByName(name string) (PlatformViewForCycle, bool)
}

// ValidateUpstreamRefNoCycle checks that an upstream proxy selector does not
// create a cycle in the subscription upstream reference graph.
//
// Two selector kinds are followed:
//   - ref:<hash>      → the node's owning subscriptions become dependencies;
//   - platform:<name> → every subscription owning a node in that platform's
//     routable view becomes a dependency (resolving the platform picks one of
//     them, so any of them can close the loop).
//
// Returns nil if no cycle is detected, or an error describing the cycle.
// URI upstreams and empty upstreams are not checked (they point outside Resin
// and cannot form Resin-internal cycles).
func ValidateUpstreamRefNoCycle(
	startSubID string,
	upstream string,
	subLookup SubscriptionLookupForCycle,
	nodeLookup NodeLookupForCycle,
) error {
	if !IsUpstreamRef(upstream) && !IsUpstreamPlatform(upstream) {
		return nil
	}

	visited := map[string]bool{startSubID: true}
	return walkUpstreamRefChain(startSubID, upstream, subLookup, nodeLookup, visited, 0)
}

// upstreamRefDepthError reports an over-deep upstream reference chain.
type upstreamRefDepthError struct {
	maxDepth int
	startSub string
}

func (e upstreamRefDepthError) Error() string {
	return fmt.Sprintf("upstream ref chain exceeds maximum depth %d starting from subscription %s", e.maxDepth, e.startSub)
}

// walkUpstreamRefChain follows a ref:<hash> chain. It also follows
// platform:<name> edges: a subscription that uses a platform as its upstream
// depends on every subscription that owns a node in that platform's routable
// view, so an indirect cycle (A -platform-> B's node, B -ref-> A's node) is
// detected here as well.
func walkUpstreamRefChain(
	currentSubID string,
	upstream string,
	subLookup SubscriptionLookupForCycle,
	nodeLookup NodeLookupForCycle,
	visited map[string]bool,
	depth int,
) error {
	if depth > maxUpstreamRefDepth {
		return upstreamRefDepthError{maxDepth: maxUpstreamRefDepth, startSub: currentSubID}
	}

	if IsUpstreamPlatform(upstream) {
		return walkPlatformUpstreamChain(currentSubID, upstream, subLookup, nodeLookup, visited, depth)
	}

	hashHex := ParseUpstreamRefHash(upstream)
	if hashHex == "" {
		return nil // not a valid ref, nothing to check
	}

	hash, err := node.ParseHex(hashHex)
	if err != nil {
		return nil // invalid hash, will fail at refresh time
	}

	entry, ok := nodeLookup.GetEntry(hash)
	if !ok {
		return nil // node not in pool, will fail at refresh time
	}

	// Check all subscriptions that own this node.
	var cycleErr error
	for _, subID := range entry.SubscriptionIDs() {
		if subID == currentSubID {
			// Node belongs to the same subscription that references it.
			// This is a self-loop: A → ref:nodeX → nodeX belongs to A.
			cycleErr = fmt.Errorf("upstream ref cycle: subscription %s references a node that belongs to itself", subID)
			break
		}
		if visited[subID] {
			// Already visited this subscription → cycle.
			cycleErr = fmt.Errorf("upstream ref cycle: subscription %s is part of a cycle in the upstream reference chain", subID)
			break
		}

		sub, found := subLookup.Get(subID)
		if !found {
			continue // subscription not found, skip
		}
		nextUpstream := sub.UpstreamProxy()
		if !IsUpstreamRef(nextUpstream) && !IsUpstreamPlatform(nextUpstream) {
			continue // neither a ref nor a platform: cannot form a cycle through this path
		}

		visited[subID] = true
		cycleErr = walkUpstreamRefChain(subID, nextUpstream, subLookup, nodeLookup, visited, depth+1)
		if cycleErr != nil {
			break
		}
		delete(visited, subID) // backtrack for other paths
	}

	return cycleErr
}

// walkPlatformUpstreamChain expands a platform:<name> upstream edge.
//
// A platform upstream does not name a single node: at refresh time it resolves
// to whichever routable node has the lowest latency, which may belong to any
// subscription feeding that platform. Conservatively treating every such
// subscription as a dependency detects indirect cycles that a ref-only walk
// would miss (A -platform-> node of B, B -ref-> node of A).
//
// The check is intentionally conservative: a cycle report may be a false
// positive when the resolver would not actually pick the offending node. That
// is preferable to shipping a configuration that can deadlock both
// subscriptions at refresh time.
func walkPlatformUpstreamChain(
	currentSubID string,
	upstream string,
	subLookup SubscriptionLookupForCycle,
	nodeLookup NodeLookupForCycle,
	visited map[string]bool,
	depth int,
) error {
	platName := ParsePlatformName(upstream)
	if platName == "" {
		return nil // malformed selector, rejected elsewhere
	}
	platLookup, ok := nodeLookup.(PlatformLookupForCycle)
	if !ok {
		return nil // no platform access: skip the check
	}
	plat, found := platLookup.GetPlatformByName(platName)
	if !found {
		return nil // unknown platform, reported at refresh time
	}

	// Collect the subscriptions that own nodes in the platform's view.
	var deps []string
	seen := make(map[string]struct{})
	plat.View().Range(func(h node.Hash) bool {
		entry, ok := nodeLookup.GetEntry(h)
		if !ok || entry == nil {
			return true
		}
		for _, subID := range entry.SubscriptionIDs() {
			if subID == currentSubID {
				// The platform contains this subscription's own nodes. Picking
				// one would make nodes dial through themselves.
				continue
			}
			if _, dup := seen[subID]; dup {
				continue
			}
			seen[subID] = struct{}{}
			deps = append(deps, subID)
		}
		return true
	})

	for _, subID := range deps {
		if visited[subID] {
			return fmt.Errorf("upstream ref cycle: subscription %s is reachable again through platform %q", subID, platName)
		}
		sub, found := subLookup.Get(subID)
		if !found {
			continue
		}
		nextUpstream := sub.UpstreamProxy()
		if !IsUpstreamRef(nextUpstream) && !IsUpstreamPlatform(nextUpstream) {
			continue
		}
		visited[subID] = true
		if err := walkUpstreamRefChain(subID, nextUpstream, subLookup, nodeLookup, visited, depth+1); err != nil {
			return err
		}
		delete(visited, subID)
	}
	return nil
}
