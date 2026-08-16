package routing_test

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/routing"
)

// zeroNodeHashHex is a valid 16-byte hex node hash usable by UpsertLease and
// RestoreLeases when the node does not need to exist in the pool.
const zeroNodeHashHex = "00000000000000000000000000000000"

func mustAddr(ip string) netip.Addr {
	return netip.MustParseAddr(ip)
}

// setupQuotaRouter builds a router over a platform with the per-IP account
// quota enabled (max/window) and nIP distinct egress IPs.
func setupQuotaRouter(t testing.TB, max int, window time.Duration, nIP int) *routing.Router {
	t.Helper()
	pool, subMgr := setupPool(t)

	plat, ok := pool.GetPlatform(platID)
	if !ok {
		t.Fatalf("platform %s not found", platID)
	}
	plat.MaxAccountsPerIP = max
	plat.IPAccountWindowNs = int64(window)

	for i := 0; i < nIP; i++ {
		raw := fmt.Sprintf(`{"ipquota":"node-%d"}`, i)
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		makeRoutableNode(t, pool, subMgr, raw, ip, "cloudflare.com", 50*time.Millisecond)
	}
	return makeRouter(pool, nil)
}

func TestIPQuota_DisabledByDefault_NoBehaviorChange(t *testing.T) {
	// max=0 → quota off: many distinct accounts all bind the single IP.
	router := setupQuotaRouter(t, 0, 2*time.Hour, 1)

	for i := 0; i < 6; i++ {
		acc := string(rune('a' + i))
		if _, err := router.RouteRequest(platName, acc, "example.com"); err != nil {
			t.Fatalf("account %s: unexpected error %v", acc, err)
		}
	}

	snap := router.SnapshotIPQuota(platID, int64(2*time.Hour))
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.AccountsByIP) != 0 {
		t.Fatalf("window should stay empty when quota disabled, got %v", snap.AccountsByIP)
	}
}

func TestIPQuota_FourthAccountAvoidsFullIP(t *testing.T) {
	const max = 2
	router := setupQuotaRouter(t, max, 2*time.Hour, 3)

	routed := make(map[string]string) // account -> ip
	for _, acc := range []string{"a", "b", "c", "d", "e"} {
		res, err := router.RouteRequest(platName, acc, "example.com")
		if err != nil {
			t.Fatalf("account %s: unexpected error %v", acc, err)
		}
		routed[acc] = res.EgressIP.String()
	}

	perIP := make(map[string]int)
	for _, ip := range routed {
		perIP[ip]++
	}
	if len(perIP) < 2 {
		t.Fatalf("expected spreading across IPs, mapping %v", routed)
	}
	for ip, n := range perIP {
		if n > max+1 {
			t.Fatalf("ip %s bound %d accounts, exceeds fail-open ceiling %d", ip, n, max+1)
		}
	}

	snap := router.SnapshotIPQuota(platID, int64(2*time.Hour))
	for ip, n := range snap.AccountsByIP {
		if n > max+1 {
			t.Fatalf("window accounts for %s = %d, exceeds ceiling %d", ip, n, max+1)
		}
	}
}

func TestIPQuota_SameAccountRepeatSingleCount(t *testing.T) {
	router := setupQuotaRouter(t, 2, 2*time.Hour, 1)

	res1, err := router.RouteRequest(platName, "acc1", "example.com")
	if err != nil {
		t.Fatalf("first route: %v", err)
	}
	for i := 0; i < 5; i++ {
		res, err := router.RouteRequest(platName, "acc1", "example.com")
		if err != nil {
			t.Fatalf("repeat route %d: %v", i, err)
		}
		if res.EgressIP != res1.EgressIP || res.LeaseCreated {
			t.Fatalf("repeat route %d should hit existing lease", i)
		}
	}

	snap := router.SnapshotIPQuota(platID, int64(2*time.Hour))
	if got := snap.AccountsByIP[res1.EgressIP]; got != 1 {
		t.Fatalf("window accounts = %d, want 1", got)
	}
}

func TestIPQuota_FailOpenCeilingThenFailClosed(t *testing.T) {
	// Single IP, max=2: a,b fit; c falls back (ceiling 3); d fails closed.
	router := setupQuotaRouter(t, 2, 2*time.Hour, 1)

	for _, acc := range []string{"a", "b", "c"} {
		if _, err := router.RouteRequest(platName, acc, "example.com"); err != nil {
			t.Fatalf("account %s: unexpected error %v", acc, err)
		}
	}
	if _, err := router.RouteRequest(platName, "d", "example.com"); !errors.Is(err, routing.ErrNoAvailableNodes) {
		t.Fatalf("account d: err = %v, want ErrNoAvailableNodes", err)
	}

	snap := router.SnapshotIPQuota(platID, int64(2*time.Hour))
	if snap.FallbackTotal != 1 {
		t.Fatalf("fallbackTotal = %d, want 1", snap.FallbackTotal)
	}
	for ip, n := range snap.AccountsByIP {
		if n != 3 {
			t.Fatalf("ip %s window accounts = %d, want 3 (max+1)", ip, n)
		}
	}
}

func TestIPQuota_WindowSlideRestoresQuota(t *testing.T) {
	window := 40 * time.Millisecond
	router := setupQuotaRouter(t, 2, window, 1)

	for _, acc := range []string{"a", "b"} {
		if _, err := router.RouteRequest(platName, acc, "example.com"); err != nil {
			t.Fatalf("account %s: %v", acc, err)
		}
	}
	if _, err := router.RouteRequest(platName, "c", "example.com"); err != nil {
		t.Fatalf("c should use fail-open fallback: %v", err)
	}
	if _, err := router.RouteRequest(platName, "d", "example.com"); !errors.Is(err, routing.ErrNoAvailableNodes) {
		t.Fatalf("d should be blocked at ceiling: %v", err)
	}

	time.Sleep(2 * window)

	// Window slid out: fresh accounts are admitted again.
	if _, err := router.RouteRequest(platName, "e", "example.com"); err != nil {
		t.Fatalf("e after window slide: %v", err)
	}
	snap := router.SnapshotIPQuota(platID, int64(window))
	for ip, n := range snap.AccountsByIP {
		if n != 1 {
			t.Fatalf("ip %s window accounts = %d, want 1 after slide", ip, n)
		}
	}
}

func TestIPQuota_ExistingLeaseNotEvicted(t *testing.T) {
	router := setupQuotaRouter(t, 2, 2*time.Hour, 2)

	resA, err := router.RouteRequest(platName, "a", "example.com")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	// Fill remaining capacity everywhere.
	for _, acc := range []string{"b", "c", "d"} {
		if _, err := router.RouteRequest(platName, acc, "example.com"); err != nil {
			t.Fatalf("%s: %v", acc, err)
		}
	}
	// a keeps its original lease even though IPs are at quota.
	res, err := router.RouteRequest(platName, "a", "example.com")
	if err != nil {
		t.Fatalf("a re-route: %v", err)
	}
	if res.EgressIP != resA.EgressIP || res.LeaseCreated {
		t.Fatal("existing lease should be kept (sticky, no eviction)")
	}
}

func TestIPQuota_RestoreLeasesSeedsWindow(t *testing.T) {
	router := setupQuotaRouter(t, 3, time.Hour, 1)

	nowNs := time.Now().UnixNano()
	hour := int64(time.Hour)
	router.RestoreLeases([]model.Lease{
		{PlatformID: platID, Account: "a", NodeHash: zeroNodeHashHex, EgressIP: "10.0.0.1", CreatedAtNs: nowNs - 60_000_000_000, ExpiryNs: nowNs + hour},
		{PlatformID: platID, Account: "b", NodeHash: zeroNodeHashHex, EgressIP: "10.0.0.1", CreatedAtNs: nowNs - 60_000_000_000, ExpiryNs: nowNs + hour},
		// Old binding outside the window: must NOT seed.
		{PlatformID: platID, Account: "old", NodeHash: zeroNodeHashHex, EgressIP: "10.0.0.1", CreatedAtNs: nowNs - 2*hour, ExpiryNs: nowNs + hour},
	})

	snap := router.SnapshotIPQuota(platID, hour)
	if got := snap.AccountsByIP[mustAddr("10.0.0.1")]; got != 2 {
		t.Fatalf("seeded window accounts = %d, want 2 (recent leases only)", got)
	}

	// max=3: only one more account fits on the normal path; the next one
	// needs the fail-open fallback.
	if _, err := router.RouteRequest(platName, "c", "example.com"); err != nil {
		t.Fatalf("c: %v", err)
	}
	if _, err := router.RouteRequest(platName, "d", "example.com"); err != nil {
		t.Fatalf("d should ride the fallback: %v", err)
	}
	if _, err := router.RouteRequest(platName, "e", "example.com"); !errors.Is(err, routing.ErrNoAvailableNodes) {
		t.Fatalf("e should hit the ceiling: %v", err)
	}
}

func TestIPQuota_UpsertLeaseConsumesQuota(t *testing.T) {
	router := setupQuotaRouter(t, 2, 2*time.Hour, 1)

	nowNs := time.Now().UnixNano()
	for _, acc := range []string{"a", "b"} {
		ml := model.Lease{
			PlatformID:  platID,
			Account:     acc,
			NodeHash:    zeroNodeHashHex,
			EgressIP:    "10.0.0.1",
			CreatedAtNs: nowNs,
			ExpiryNs:    nowNs + int64(time.Hour),
		}
		if err := router.UpsertLease(ml); err != nil {
			t.Fatalf("UpsertLease %s: %v", acc, err)
		}
	}

	snap := router.SnapshotIPQuota(platID, int64(2*time.Hour))
	if got := snap.AccountsByIP[mustAddr("10.0.0.1")]; got != 2 {
		t.Fatalf("window accounts = %d, want 2 (control-plane upserts counted)", got)
	}
}

func TestIPQuota_DeletedLeaseStillCountsInWindow(t *testing.T) {
	router := setupQuotaRouter(t, 2, 2*time.Hour, 1)

	for _, acc := range []string{"a", "b"} {
		if _, err := router.RouteRequest(platName, acc, "example.com"); err != nil {
			t.Fatalf("%s: %v", acc, err)
		}
	}
	if !router.DeleteLease(platID, "a") {
		t.Fatal("delete lease failed")
	}

	// Quota slot stays consumed until the window slides (risk-control
	// semantics), so a fresh account must go through the fallback.
	if _, err := router.RouteRequest(platName, "c", "example.com"); err != nil {
		t.Fatalf("c via fallback: %v", err)
	}
	snap := router.SnapshotIPQuota(platID, int64(2*time.Hour))
	if got := snap.AccountsByIP[mustAddr("10.0.0.1")]; got != 3 {
		t.Fatalf("window accounts = %d, want 3 (a still counted)", got)
	}
}

func TestIPQuota_AccountRotationScheduling(t *testing.T) {
	// Covers upstream account scheduling (e.g. grok2api rotating accounts on
	// the same IP): an actively rotated account never loses its slot, while a
	// lease whose entry slid out re-enters below max as a regular slot, then up
	// to max+1 via fallback; beyond the ceiling the request fails closed.
	window := 250 * time.Millisecond
	router := setupQuotaRouter(t, 2, window, 1)

	// a and b occupy both slots and hold sticky leases.
	for _, acc := range []string{"a", "b"} {
		if _, err := router.RouteRequest(platName, acc, "example.com"); err != nil {
			t.Fatalf("%s: %v", acc, err)
		}
	}

	// Keep a active across several window boundaries: every hit must refresh
	// its entry, so a never slides out. b goes idle and slides out.
	deadline := time.Now().Add(3 * window)
	for time.Now().Before(deadline) {
		res, err := router.RouteRequest(platName, "a", "example.com")
		if err != nil {
			t.Fatalf("a active rotation: %v", err)
		}
		if res.LeaseCreated || res.EgressIP != mustAddr("10.0.0.1") {
			t.Fatalf("a should keep hitting its lease on 10.0.0.1, got created=%v ip=%v", res.LeaseCreated, res.EgressIP)
		}
		time.Sleep(window / 3)
	}

	// Fresh account c fills the free slot (b's old slot) on the normal path.
	if _, err := router.RouteRequest(platName, "c", "example.com"); err != nil {
		t.Fatalf("c: %v", err)
	}

	// b returns while its lease is still alive but its entry slid out: it must
	// be re-admitted through the fallback slot (max+1), keeping its lease.
	resB, err := router.RouteRequest(platName, "b", "example.com")
	if err != nil {
		t.Fatalf("b returning: %v", err)
	}
	if resB.LeaseCreated || resB.EgressIP != mustAddr("10.0.0.1") {
		t.Fatalf("b should hit its existing lease, got created=%v ip=%v", resB.LeaseCreated, resB.EgressIP)
	}

	// d has no lease and the ceiling is reached: fail closed.
	if _, err := router.RouteRequest(platName, "d", "example.com"); !errors.Is(err, routing.ErrNoAvailableNodes) {
		t.Fatalf("d: err = %v, want ErrNoAvailableNodes", err)
	}

	snap := router.SnapshotIPQuota(platID, int64(window))
	if snap.FallbackTotal < 1 {
		t.Fatalf("fallbackTotal = %d, want >= 1", snap.FallbackTotal)
	}
	details := snap.DetailByIP[mustAddr("10.0.0.1")]
	if len(details) != 3 {
		t.Fatalf("window details = %v, want 3 entries (a, b, c)", details)
	}
	byAccount := make(map[string]routing.IPAccountEntry, len(details))
	for _, d := range details {
		byAccount[d.Account] = d
	}
	if !byAccount["b"].ViaFallback || byAccount["a"].ViaFallback || byAccount["c"].ViaFallback {
		t.Fatalf("viaFallback flags = %v, want only b fallback", byAccount)
	}
	for _, acc := range []string{"a", "b", "c"} {
		if !byAccount[acc].HasLease {
			t.Fatalf("account %s should hold a lease", acc)
		}
	}

	// Deleting c's lease leaves a residual window entry without a lease.
	if !router.DeleteLease(platID, "c") {
		t.Fatal("delete lease c failed")
	}
	snap = router.SnapshotIPQuota(platID, int64(window))
	if got := snap.AccountsByIP[mustAddr("10.0.0.1")]; got != 3 {
		t.Fatalf("window accounts after delete = %d, want 3 (c residual)", got)
	}
	for _, d := range snap.DetailByIP[mustAddr("10.0.0.1")] {
		if d.Account == "c" && d.HasLease {
			t.Fatal("c must be a residual entry (has_lease=false) after lease deletion")
		}
	}
}
