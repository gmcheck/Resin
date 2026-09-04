package subscription

import (
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
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
