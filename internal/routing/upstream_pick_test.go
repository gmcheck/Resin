package routing

import (
	"errors"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

func TestPickLowestLatencyNode_PicksLowestRecentAuthority(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-1", "Plat-1", nil, nil)
	pool.addPlatform(plat)

	slowHash, slowEntry := newRoutableEntry(t, `{"id":"slow"}`, "198.51.100.1")
	fastHash, fastEntry := newRoutableEntry(t, `{"id":"fast"}`, "198.51.100.2")
	midHash, midEntry := newRoutableEntry(t, `{"id":"mid"}`, "198.51.100.3")
	pool.addEntry(slowHash, slowEntry)
	pool.addEntry(fastHash, fastEntry)
	pool.addEntry(midHash, midEntry)

	setAuthorityLatency(t, slowEntry, 300*time.Millisecond)
	setAuthorityLatency(t, fastEntry, 80*time.Millisecond)
	setAuthorityLatency(t, midEntry, 150*time.Millisecond)

	pool.rebuildPlatformView(plat)
	router := newTestRouter(pool, nil)

	got, err := router.PickLowestLatencyNode("Plat-1")
	if err != nil {
		t.Fatalf("PickLowestLatencyNode: %v", err)
	}
	if got != fastHash {
		t.Fatalf("picked %s, want %s", got.Hex(), fastHash.Hex())
	}
}

func TestPickLowestLatencyNode_IgnoresStaleSamples(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-1", "Plat-1", nil, nil)
	pool.addPlatform(plat)

	staleHash, staleEntry := newRoutableEntry(t, `{"id":"stale"}`, "198.51.100.1")
	freshHash, freshEntry := newRoutableEntry(t, `{"id":"fresh"}`, "198.51.100.2")
	pool.addEntry(staleHash, staleEntry)
	pool.addEntry(freshHash, freshEntry)

	// Stale sample is far faster but outside the freshness window.
	setAuthorityLatency(t, staleEntry, 10*time.Millisecond)
	ageAuthoritySample(t, staleEntry, 2*time.Hour)
	setAuthorityLatency(t, freshEntry, 200*time.Millisecond)

	pool.rebuildPlatformView(plat)
	router := newTestRouter(pool, nil)

	got, err := router.PickLowestLatencyNode("Plat-1")
	if err != nil {
		t.Fatalf("PickLowestLatencyNode: %v", err)
	}
	if got != freshHash {
		t.Fatalf("picked %s, want %s (stale sample must be ignored)", got.Hex(), freshHash.Hex())
	}
}

func TestPickLowestLatencyNode_FallsBackWhenNoRecentSample(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-1", "Plat-1", nil, nil)
	pool.addPlatform(plat)

	hashA, entryA := newRoutableEntry(t, `{"id":"a"}`, "198.51.100.1")
	hashB, entryB := newRoutableEntry(t, `{"id":"b"}`, "198.51.100.2")
	pool.addEntry(hashA, entryA)
	pool.addEntry(hashB, entryB)
	ageAuthoritySample(t, entryA, 2*time.Hour)
	ageAuthoritySample(t, entryB, 2*time.Hour)

	pool.rebuildPlatformView(plat)
	router := newTestRouter(pool, nil)

	got, err := router.PickLowestLatencyNode("Plat-1")
	if err != nil {
		t.Fatalf("PickLowestLatencyNode: %v", err)
	}
	want := hashA
	if hashLess(hashB, hashA) {
		want = hashB
	}
	if got != want {
		t.Fatalf("fallback picked %s, want lowest hash %s", got.Hex(), want.Hex())
	}
}

func TestPickLowestLatencyNode_Errors(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-1", "Plat-1", nil, nil)
	pool.addPlatform(plat)
	router := newTestRouter(pool, nil)

	if _, err := router.PickLowestLatencyNode("missing"); !errors.Is(err, ErrPlatformNotFound) {
		t.Fatalf("unknown platform: got %v, want ErrPlatformNotFound", err)
	}
	if _, err := router.PickLowestLatencyNode("Plat-1"); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("empty view: got %v, want ErrNoAvailableNodes", err)
	}
}

func setAuthorityLatency(t *testing.T, entry *node.NodeEntry, d time.Duration) {
	t.Helper()
	entry.LatencyTable.LoadEntryClassified("cloudflare.com", node.DomainLatencyStats{
		Ewma:        d,
		LastUpdated: time.Now(),
	}, true)
	waitForDomainLatency(t, entry, "cloudflare.com")
}

func ageAuthoritySample(t *testing.T, entry *node.NodeEntry, age time.Duration) {
	t.Helper()
	entry.LatencyTable.LoadEntryClassified("cloudflare.com", node.DomainLatencyStats{
		Ewma:        10 * time.Millisecond,
		LastUpdated: time.Now().Add(-age),
	}, true)
	waitForDomainLatency(t, entry, "cloudflare.com")
}

func TestPickLowestLatencyNodeExcluding_SkipsOwnSubscriptionNodes(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-1", "Plat-1", nil, nil)
	pool.addPlatform(plat)

	// The fastest node belongs to the subscription itself and must be skipped.
	ownHash, ownEntry := newRoutableEntry(t, `{"id":"own"}`, "198.51.100.1")
	otherHash, otherEntry := newRoutableEntry(t, `{"id":"other"}`, "198.51.100.2")
	pool.addEntry(ownHash, ownEntry)
	pool.addEntry(otherHash, otherEntry)

	ownEntry.AddSubscriptionID("sub-self")
	setAuthorityLatency(t, ownEntry, 20*time.Millisecond)
	setAuthorityLatency(t, otherEntry, 200*time.Millisecond)

	pool.rebuildPlatformView(plat)
	router := newTestRouter(pool, nil)

	got, err := router.PickLowestLatencyNodeExcluding("Plat-1", "sub-self")
	if err != nil {
		t.Fatalf("PickLowestLatencyNodeExcluding: %v", err)
	}
	if got == ownHash {
		t.Fatal("picked a node owned by the excluded subscription (self-loop)")
	}
	if got != otherHash {
		t.Fatalf("picked %s, want %s", got.Hex(), otherHash.Hex())
	}
}

func TestPickLowestLatencyNodeExcluding_AllOwnedReturnsError(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-1", "Plat-1", nil, nil)
	pool.addPlatform(plat)

	onlyHash, onlyEntry := newRoutableEntry(t, `{"id":"only"}`, "198.51.100.1")
	pool.addEntry(onlyHash, onlyEntry)
	onlyEntry.AddSubscriptionID("sub-self")
	pool.rebuildPlatformView(plat)

	router := newTestRouter(pool, nil)
	if _, err := router.PickLowestLatencyNodeExcluding("Plat-1", "sub-self"); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("got %v, want ErrNoAvailableNodes", err)
	}
}
