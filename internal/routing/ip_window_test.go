package routing

import (
	"net/netip"
	"sync"
	"testing"
)

var testIP = netip.MustParseAddr("1.2.3.4")

func TestIPAccountWindow_TouchIdempotent(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	w.Touch(testIP, "a", now)
	w.Touch(testIP, "a", now+1_000) // re-touch must not refresh firstSeen
	w.Touch(testIP, "b", now+500)

	if got := w.CountRecent(testIP, now, win); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	// First entry slides out at now+win using the ORIGINAL firstSeen.
	if got := w.CountRecent(testIP, now+win-1, win); got != 2 {
		t.Fatalf("count before slide = %d, want 2", got)
	}
	if got := w.CountRecent(testIP, now+win, win); got != 1 {
		t.Fatalf("count after slide = %d, want 1 (b only)", got)
	}
}

func TestIPAccountWindow_InvalidInput(t *testing.T) {
	w := NewIPAccountWindow()
	w.Touch(netip.Addr{}, "a", 1)
	w.Touch(testIP, "", 1)
	if got := w.CountRecent(testIP, 1, 100); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}

func TestIPAccountWindow_ReserveIfEligible(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	// Slots 1..2.
	if !w.ReserveIfEligible(testIP, "a", now, win, 2) {
		t.Fatal("a should be eligible")
	}
	if !w.ReserveIfEligible(testIP, "b", now+1, win, 2) {
		t.Fatal("b should be eligible")
	}
	// Slot exhausted by a NEW account.
	if w.ReserveIfEligible(testIP, "c", now+2, win, 2) {
		t.Fatal("c should be blocked")
	}
	// Already-counted accounts stay eligible (re-bind keeps firstSeen).
	if !w.ReserveIfEligible(testIP, "a", now+3, win, 2) {
		t.Fatal("existing account a should stay eligible")
	}
	if got := w.CountRecent(testIP, now+3, win); got != 2 {
		t.Fatalf("count = %d, want 2 (a,b)", got)
	}
	// Window slides → c fits.
	if !w.ReserveIfEligible(testIP, "c", now+win+1, win, 2) {
		t.Fatal("c should be eligible after window slide")
	}

	blocked, _ := w.Stats()
	if blocked != 1 {
		t.Fatalf("blockedTotal = %d, want 1", blocked)
	}
}

func TestIPAccountWindow_ReserveForcedFallbackCeiling(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	for _, acc := range []string{"a", "b"} {
		if !w.ReserveIfEligible(testIP, acc, now, win, 2) {
			t.Fatalf("%s should be eligible", acc)
		}
	}
	// Fail-open overshoot: max+1 = 3 is allowed.
	if !w.ReserveForcedFallback(testIP, "c", now+1, win, 2) {
		t.Fatal("c should be force-reserved under ceiling")
	}
	// Hard ceiling reached: a 4th distinct account is refused.
	if w.ReserveForcedFallback(testIP, "d", now+2, win, 2) {
		t.Fatal("d should hit the hard ceiling")
	}
	// Already-counted accounts still pass the fallback path.
	if !w.ReserveForcedFallback(testIP, "c", now+3, win, 2) {
		t.Fatal("counted account c should pass")
	}

	_, fallback := w.Stats()
	if fallback != 1 {
		t.Fatalf("fallbackTotal = %d, want 1", fallback)
	}
}

func TestIPAccountWindow_PruneAndCountsByIP(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(100)

	ip2 := netip.MustParseAddr("5.6.7.8")
	w.Touch(testIP, "a", now)
	w.Touch(testIP, "b", now+10)
	w.Touch(ip2, "c", now+10)

	counts := w.CountsByIP(now+50, win)
	if len(counts) != 2 || counts[testIP] != 2 || counts[ip2] != 1 {
		t.Fatalf("counts = %v, want {ip:2, ip2:1}", counts)
	}

	w.Prune(now+105, win) // only a (firstSeen=now) slides out; cutoff=now+5
	counts = w.CountsByIP(now+105, win)
	if len(counts) != 2 || counts[testIP] != 1 || counts[ip2] != 1 {
		t.Fatalf("counts after prune = %v, want {ip:1, ip2:1}", counts)
	}

	w.Prune(now+win+20, win) // everything slides out
	if counts = w.CountsByIP(now+win+20, win); len(counts) != 0 {
		t.Fatalf("counts after full prune = %v, want empty", counts)
	}
}

func TestIPAccountWindow_ConcurrentReserveNoOvershoot(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)
	max := 10

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		acc := string(rune('a'+i%26)) + string(rune('0'+i/26))
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.ReserveIfEligible(testIP, acc, now, win, max)
		}()
	}
	wg.Wait()
	if got := w.CountRecent(testIP, now, win); got != max {
		t.Fatalf("count = %d, want exactly max=%d (no overshoot)", got, max)
	}
}
