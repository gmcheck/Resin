package subscription

import (
	"fmt"
	"testing"
	"time"

	"math/rand/v2"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

type stubSubLookup struct{ subs map[string]*Subscription }

func (s stubSubLookup) Get(id string) (*Subscription, bool) {
	sub, ok := s.subs[id]
	return sub, ok
}

type stubNodeLookup struct{ entries map[node.Hash]*node.NodeEntry }

func (s stubNodeLookup) GetEntry(hash node.Hash) (*node.NodeEntry, bool) {
	e, ok := s.entries[hash]
	return e, ok
}

func hashHex(i int) string { return fmt.Sprintf("%032x", i) }

func mustHash(t *testing.T, hex string) node.Hash {
	t.Helper()
	h, err := node.ParseHex(hex)
	if err != nil {
		t.Fatalf("ParseHex(%q): %v", hex, err)
	}
	return h
}

func mustEntry(t *testing.T, hex string, subIDs ...string) *node.NodeEntry {
	t.Helper()
	e := node.NewNodeEntry(mustHash(t, hex), nil, time.Time{}, 0)
	for _, id := range subIDs {
		e.AddSubscriptionID(id)
	}
	return e
}

func newSub(id string, upstream string) *Subscription {
	s := NewSubscription(id, id, "https://example.com/"+id, true, false)
	s.SetUpstreamProxy(upstream)
	return s
}

func validate(t *testing.T, id string, upstream string, subs stubSubLookup, nodes stubNodeLookup) error {
	t.Helper()
	return ValidateUpstreamRefNoCycle(id, upstream, subs, nodes)
}

func TestValidateUpstreamRefNoCycle_SelfLoop(t *testing.T) {
	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "ref:"+hashHex(1)),
	}}
	entries := stubNodeLookup{entries: map[node.Hash]*node.NodeEntry{
		mustHash(t, hashHex(1)): mustEntry(t, hashHex(1), "A"),
	}}
	if err := validate(t, "A", "ref:"+hashHex(1), subs, entries); err == nil {
		t.Fatalf("expected self-loop rejection, but got nil")
	}
}

func TestValidateUpstreamRefNoCycle_TwoSubCycle(t *testing.T) {
	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "ref:"+hashHex(2)),
		"B": newSub("B", "ref:"+hashHex(1)),
	}}
	entries := stubNodeLookup{entries: map[node.Hash]*node.NodeEntry{
		mustHash(t, hashHex(1)): mustEntry(t, hashHex(1), "A"),
		mustHash(t, hashHex(2)): mustEntry(t, hashHex(2), "B"),
	}}
	if err := validate(t, "A", "ref:"+hashHex(2), subs, entries); err == nil {
		t.Fatalf("expected cycle rejection,but got nil")
	}
}

func TestValidateUpstreamRefNoCycle_NoCycleThroughURI(t *testing.T) {
	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "ref:"+hashHex(2)),
		"B": newSub("B", "ref:"+hashHex(3)),
		"C": newSub("C", "socks5://127.0.0.1:1080"),
	}}
	entries := stubNodeLookup{entries: map[node.Hash]*node.NodeEntry{
		mustHash(t, hashHex(2)): mustEntry(t, hashHex(2), "B"),
		mustHash(t, hashHex(3)): mustEntry(t, hashHex(3), "C"),
	}}
	if err := validate(t, "A", "ref:"+hashHex(2), subs, entries); err != nil {
		t.Fatalf("expected no cycle, but got:%v", err)
	}
}

func TestValidateUpstreamRefNoCycle_MultiOwnerNodeCycle(t *testing.T) {
	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "ref:"+hashHex(1)),
		"B": newSub("B", "ref:"+hashHex(2)),
		"C": newSub("C", "socks5://127.0.0.1:1080"),
	}}
	entries := stubNodeLookup{entries: map[node.Hash]*node.NodeEntry{
		mustHash(t, hashHex(1)): mustEntry(t, hashHex(1), "B", "C"),
		mustHash(t, hashHex(2)): mustEntry(t, hashHex(2), "A"),
	}}
	if err := validate(t, "A", "ref:"+hashHex(1), subs, entries); err == nil {
		t.Fatalf("expected cycle rejection via multi-owner node,but got nil")
	}
}

func TestValidateUpstreamRefNoCycle_DepthLimit(t *testing.T) {
	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "ref:"+hashHex(1)),
	}}
	entries := stubNodeLookup{entries: map[node.Hash]*node.NodeEntry{
		mustHash(t, hashHex(1)): mustEntry(t, hashHex(1), "S1"),
	}}
	for i := 1; i <= 10; i++ {
		subID := fmt.Sprintf("S%d", i)
		subs.subs[subID] = newSub(subID, "ref:"+hashHex(i+1))
		entries.entries[mustHash(t, hashHex(i+1))] = mustEntry(t, hashHex(i+1), fmt.Sprintf("S%d", i+1))
	}
	subs.subs["S11"] = newSub("S11", "ref:"+hashHex(12))
	if err := validate(t, "A", "ref:"+hashHex(1), subs, entries); err == nil {
		t.Fatalf("expected depth-limit rejection,but got nil")
	}
}

func TestValidateUpstreamRefNoCycle_NonRefNoop(t *testing.T) {
	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "socks5://127.0.0.1:1080"),
	}}
	entries := stubNodeLookup{entries: map[node.Hash]*node.NodeEntry{}}
	for _, upstream := range []string{"socks5://127.0.0.1:1080", "", "ref:nothex", "ref:abc"} {
		if err := validate(t, "A", upstream, subs, entries); err != nil {
			t.Fatalf("expected nil for upstream %q, but got:%v", upstream, err)
		}
	}
}

// stubPlatformLookup exposes a single platform so platform upstream edges can
// be expanded during cycle detection.
type stubPlatformLookup struct {
	entries map[node.Hash]*node.NodeEntry
	plats   map[string]PlatformViewForCycle
}

func (s stubPlatformLookup) GetEntry(hash node.Hash) (*node.NodeEntry, bool) {
	e, ok := s.entries[hash]
	return e, ok
}

func (s stubPlatformLookup) GetPlatformByName(name string) (PlatformViewForCycle, bool) {
	p, ok := s.plats[name]
	return p, ok
}

// stubPlatformView is a routable-view stand-in that only needs to iterate and
// test membership; cycle detection never inspects node health itself.
type stubPlatformView struct {
	hashes []node.Hash
}

func (v stubPlatformView) Contains(h node.Hash) bool {
	for _, c := range v.hashes {
		if c == h {
			return true
		}
	}
	return false
}

func (v stubPlatformView) Size() int { return len(v.hashes) }

func (v stubPlatformView) RandomPick(*rand.Rand) (node.Hash, bool) {
	if len(v.hashes) == 0 {
		return node.Zero, false
	}
	return v.hashes[0], true
}

func (v stubPlatformView) Range(fn func(node.Hash) bool) {
	for _, h := range v.hashes {
		if !fn(h) {
			return
		}
	}
}

// stubPlatform implements PlatformViewForCycle around a fixed hash set.
type stubPlatform struct {
	hashes []node.Hash
}

func (p stubPlatform) View() platform.ReadOnlyView {
	return stubPlatformView{hashes: p.hashes}
}

// newPlatformWithNodes builds a platform view containing exactly the given
// hashes.
func newPlatformWithNodes(t *testing.T, name string, hashes ...node.Hash) PlatformViewForCycle {
	t.Helper()
	return stubPlatform{hashes: hashes}
}

func TestValidateUpstreamRefNoCycle_PlatformIndirectCycle(t *testing.T) {
	// A uses platform P; P contains a node owned by B; B refs a node of A.
	aNode := mustHash(t, hashHex(1))
	bNode := mustHash(t, hashHex(2))

	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "platform:P"),
		"B": newSub("B", "ref:"+aNode.Hex()),
	}}
	nodes := stubPlatformLookup{
		entries: map[node.Hash]*node.NodeEntry{
			aNode: mustEntry(t, aNode.Hex(), "A"),
			bNode: mustEntry(t, bNode.Hex(), "B"),
		},
		plats: map[string]PlatformViewForCycle{
			"P": newPlatformWithNodes(t, "P", bNode),
		},
	}

	if err := ValidateUpstreamRefNoCycle("A", "platform:P", subs, nodes); err == nil {
		t.Fatal("expected an indirect cycle through the platform to be detected")
	}
}

func TestValidateUpstreamRefNoCycle_PlatformNoCycle(t *testing.T) {
	// A uses platform P; P only contains nodes of B, and B is a direct
	// subscription with no upstream. No cycle.
	bNode := mustHash(t, hashHex(2))

	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "platform:P"),
		"B": newSub("B", ""),
	}}
	nodes := stubPlatformLookup{
		entries: map[node.Hash]*node.NodeEntry{
			bNode: mustEntry(t, bNode.Hex(), "B"),
		},
		plats: map[string]PlatformViewForCycle{
			"P": newPlatformWithNodes(t, "P", bNode),
		},
	}

	if err := ValidateUpstreamRefNoCycle("A", "platform:P", subs, nodes); err != nil {
		t.Fatalf("unexpected cycle: %v", err)
	}
}

func TestValidateUpstreamRefNoCycle_PlatformOwnNodesIgnored(t *testing.T) {
	// A uses platform P and P only contains A's own nodes: the resolver skips
	// them, so this is a configuration error at refresh time but not a cycle.
	ownNode := mustHash(t, hashHex(3))

	subs := stubSubLookup{subs: map[string]*Subscription{
		"A": newSub("A", "platform:P"),
	}}
	nodes := stubPlatformLookup{
		entries: map[node.Hash]*node.NodeEntry{
			ownNode: mustEntry(t, ownNode.Hex(), "A"),
		},
		plats: map[string]PlatformViewForCycle{
			"P": newPlatformWithNodes(t, "P", ownNode),
		},
	}

	if err := ValidateUpstreamRefNoCycle("A", "platform:P", subs, nodes); err != nil {
		t.Fatalf("own nodes must not be reported as a cycle: %v", err)
	}
}
