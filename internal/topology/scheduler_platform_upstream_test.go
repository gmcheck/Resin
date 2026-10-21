package topology

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

// fakePicker resolves a platform name to a fixed hash.
type fakePicker struct {
	hash         node.Hash
	err          error
	excludeSubID string
	platformName string
}

func (f *fakePicker) PickLowestLatencyNodeExcluding(platformName string, excludeSubID string) (node.Hash, error) {
	f.platformName = platformName
	f.excludeSubID = excludeSubID
	return f.hash, f.err
}

const platformUpstreamSubID = "s-platform"

func newPlatformUpstreamFixture(t *testing.T, picker UpstreamNodePicker, upstream string) (*SubscriptionScheduler, *subscription.Subscription, *GlobalNodePool) {
	t.Helper()
	subMgr := NewSubscriptionManager()
	pool := newTestPool(subMgr)

	body := makeSubscriptionJSON(
		`{"type":"shadowsocks","tag":"hk-1","server":"1.1.1.1","server_port":443}`,
	)
	sub := subscription.NewSubscription(platformUpstreamSubID, "PlatformSub", "http://example.com", true, false)
	sub.SetFetchConfig(sub.URL(), int64(time.Hour))
	sub.SetUpstreamProxy(upstream)
	subMgr.Register(sub)

	sched := newTestScheduler(subMgr, pool, makeMockFetcher(body, nil))
	sched.SetUpstreamNodePicker(picker)
	return sched, sub, pool
}

func TestScheduler_PlatformUpstream_InjectsResolvedRef(t *testing.T) {
	target := node.HashFromRawOptions([]byte(`{"id":"upstream-target"}`))
	sched, sub, pool := newPlatformUpstreamFixture(t, &fakePicker{hash: target}, "platform:HK")

	sched.UpdateSubscription(sub)

	if err := sub.GetLastError(); err != "" {
		t.Fatalf("unexpected last_error: %s", err)
	}
	if pool.Size() != 1 {
		t.Fatalf("expected 1 node, got %d", pool.Size())
	}
	if got := sub.UpstreamResolvedHash(); got != target.Hex() {
		t.Fatalf("resolved hash: got %q, want %q", got, target.Hex())
	}
	// The injected detour must point at the resolved node, not the platform tag.
	pool.Range(func(h node.Hash, entry *node.NodeEntry) bool {
		// The ref: prefix is stripped on injection: the detour value is the
		// node hash, which is also the tag used to register the node outbound.
		if !strings.Contains(string(entry.RawOptions), `"detour":"`+target.Hex()+`"`) {
			t.Fatalf("raw options missing resolved detour: %s", entry.RawOptions)
		}
		return true
	})
}

func TestScheduler_PlatformUpstream_EmptyViewSchedulesRetry(t *testing.T) {
	sched, sub, pool := newPlatformUpstreamFixture(t, &fakePicker{err: errNoNodes()}, "platform:HK")

	sched.UpdateSubscription(sub)

	if pool.Size() != 0 {
		t.Fatalf("expected no nodes applied, got %d", pool.Size())
	}
	errMsg := sub.GetLastError()
	if !strings.Contains(errMsg, "no routable node yet") {
		t.Fatalf("last_error should explain the empty platform: %q", errMsg)
	}
	if sub.UpstreamRetryAtNs() == 0 {
		t.Fatal("transient failure should schedule an early retry")
	}
	// The early retry makes waiting a full interval unnecessary, so the normal
	// last-checked timestamp must not gate recovery.
	if got := sub.UpstreamRetryAtNs(); got <= time.Now().UnixNano() {
		t.Fatalf("retry deadline should be in the future: %d", got)
	}
}

func TestScheduler_PlatformUpstream_UnknownPlatformDoesNotScheduleRetry(t *testing.T) {
	sched, sub, _ := newPlatformUpstreamFixture(t, &fakePicker{err: errors.New("platform not found")}, "platform:missing")

	sched.UpdateSubscription(sub)

	if !strings.Contains(sub.GetLastError(), "platform not found") {
		t.Fatalf("last_error: %q", sub.GetLastError())
	}
	if sub.UpstreamRetryAtNs() != 0 {
		t.Fatal("permanent configuration error must not schedule an early retry")
	}
}

func TestScheduler_PlatformUpstream_StaleDetection(t *testing.T) {
	target := node.HashFromRawOptions([]byte(`{"id":"upstream-target"}`))
	sched, sub, pool := newPlatformUpstreamFixture(t, &fakePicker{hash: target}, "platform:HK")

	plat := platform.NewPlatform("plat-hk", "HK", nil, nil)
	pool.RegisterPlatform(plat)

	// Resolved but not in the view yet.
	sub.SetUpstreamResolvedHash(target.Hex())
	if !sched.platformUpstreamStale(sub) {
		t.Fatal("node missing from the platform view should be considered stale")
	}

	// Pretend a refresh just happened: the cooldown must suppress re-triggering.
	sub.LastCheckedNs.Store(time.Now().UnixNano())
	if sched.platformUpstreamStale(sub) {
		t.Fatal("cooldown should suppress stale re-trigger right after a refresh")
	}

	// After the cooldown the staleness is reported again.
	sub.LastCheckedNs.Store(time.Now().Add(-2 * platformStaleRefreshCooldown).UnixNano())
	if !sched.platformUpstreamStale(sub) {
		t.Fatal("staleness should be reported after the cooldown")
	}

	// Non-platform upstreams are never considered stale.
	sub.SetUpstreamProxy("socks5://127.0.0.1:1080")
	if sched.platformUpstreamStale(sub) {
		t.Fatal("URI upstreams must not be checked for platform staleness")
	}
}

func errNoNodes() error {
	return routing.ErrNoAvailableNodes
}

// A platform name may contain non-ASCII characters; the platform selector must
// resolve it byte-for-byte (no case folding, no transliteration).
func TestScheduler_PlatformUpstream_ChinesePlatformName(t *testing.T) {
	subMgr := NewSubscriptionManager()
	pool := newTestPool(subMgr)

	// Provider owns the nodes that the platform exposes.
	providerBody := makeSubscriptionJSON(
		`{"type":"shadowsocks","tag":"hk-1","server":"1.1.1.1","server_port":443}`,
		`{"type":"shadowsocks","tag":"jp-1","server":"2.2.2.2","server_port":443}`,
	)
	provider := subscription.NewSubscription("s-provider", "provider", "http://provider.local", true, false)
	provider.SetFetchConfig(provider.URL(), int64(time.Hour))
	provider.SetSourceType(subscription.SourceTypeLocal)
	provider.SetContent(string(providerBody))
	subMgr.Register(provider)

	consumerBody := makeSubscriptionJSON(
		`{"type":"shadowsocks","tag":"c-1","server":"3.3.3.3","server_port":443}`,
	)
	consumer := subscription.NewSubscription("s-consumer", "consumer", "http://consumer.local", true, false)
	consumer.SetFetchConfig(consumer.URL(), int64(time.Hour))
	consumer.SetSourceType(subscription.SourceTypeLocal)
	consumer.SetContent(string(consumerBody))
	consumer.SetUpstreamProxy("platform:前置代理")
	subMgr.Register(consumer)

	plat := platform.NewPlatform("plat-cn", "前置代理", nil, nil)
	pool.RegisterPlatform(plat)

	picker := &fakePicker{err: routing.ErrNoAvailableNodes}
	sched := NewSubscriptionScheduler(SchedulerConfig{SubManager: subMgr, Pool: pool})
	sched.SetUpstreamNodePicker(picker)

	// Provider nodes land and become routable.
	sched.UpdateSubscription(provider)
	if err := provider.GetLastError(); err != "" {
		t.Fatalf("provider refresh failed: %s", err)
	}
	seed := 0
	pool.Range(func(h node.Hash, entry *node.NodeEntry) bool {
		entry.CircuitOpenSince.Store(0)
		ob := testutil.NewNoopOutbound()
		entry.Outbound.Store(&ob)
		entry.SetEgressIP(netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", seed+1)))
		entry.LatencyTable.LoadEntryClassified("cloudflare.com", node.DomainLatencyStats{
			Ewma:        time.Duration(60+seed) * time.Millisecond,
			LastUpdated: time.Now(),
		}, true)
		seed++
		return true
	})
	pool.RebuildAllPlatforms()
	if plat.View().Size() == 0 {
		t.Fatal("platform view is empty after seeding")
	}

	// The picker records the platform name it receives.
	picker.err = nil
	picker.hash = node.Zero
	plat.View().Range(func(h node.Hash) bool {
		picker.hash = h
		return false
	})

	sched.UpdateSubscription(consumer)
	if err := consumer.GetLastError(); err != "" {
		t.Fatalf("consumer refresh failed: %s", err)
	}
	if picker.platformName != "前置代理" {
		t.Fatalf("picker received platform name %q, want %q", picker.platformName, "前置代理")
	}
	if picker.excludeSubID != consumer.ID {
		t.Fatalf("picker exclude sub: got %q, want %q", picker.excludeSubID, consumer.ID)
	}
	resolved := consumer.UpstreamResolvedHash()
	if resolved == "" {
		t.Fatal("platform upstream not resolved for a Chinese platform name")
	}
}
