package subscription

import (
	"fmt"

	"github.com/Resinat/Resin/internal/node"
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

// ValidateUpstreamRefNoCycle checks that a "ref:<hash>" upstream proxy does
// not create a cycle in the subscription upstream reference graph.
//
// The cycle detection traverses: subscription A's upstream = ref:hashX →
// find hashX's subscriptions (NodeEntry.SubscriptionIDs()) → for each
// subscription B, check B's upstream → if ref:hashY, recurse → if we
// revisit a subscription we've already seen, it's a cycle.
//
// Returns nil if no cycle is detected, or an error describing the cycle.
// Non-ref upstreams and empty upstreams are not checked (they can't form
// Resin-internal cycles).
func ValidateUpstreamRefNoCycle(
	startSubID string,
	upstream string,
	subLookup SubscriptionLookupForCycle,
	nodeLookup NodeLookupForCycle,
) error {
	if !IsUpstreamRef(upstream) {
		return nil
	}

	visited := map[string]bool{startSubID: true}
	return walkUpstreamRefChain(startSubID, upstream, subLookup, nodeLookup, visited, 0)
}

func walkUpstreamRefChain(
	currentSubID string,
	upstream string,
	subLookup SubscriptionLookupForCycle,
	nodeLookup NodeLookupForCycle,
	visited map[string]bool,
	depth int,
) error {
	if depth > maxUpstreamRefDepth {
		return fmt.Errorf("upstream ref chain exceeds maximum depth %d starting from subscription %s", maxUpstreamRefDepth, currentSubID)
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
		if !IsUpstreamRef(nextUpstream) {
			continue // not a ref, can't form a cycle through this path
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
