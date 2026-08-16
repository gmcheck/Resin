package routing

import (
	"net/netip"
	"sync"
	"testing"
)

var testIP = netip.MustParseAddr("1.2.3.4")

func TestIPAccountWindow_TouchRefreshesLastSeen(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	w.Touch(testIP, "a", now)
	w.Touch(testIP, "a", now+1_000) // activity refreshes lastSeen (recent-activity semantics)
	w.Touch(testIP, "b", now+500)

	if got := w.CountRecent(testIP, now, win); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	// Entries slide out at their own lastSeen + win.
	if got := w.CountRecent(testIP, now+win, win); got != 2 {
		t.Fatalf("count at now+win = %d, want 2 (both still in window)", got)
	}
	if got := w.CountRecent(testIP, now+500+win, win); got != 1 {
		t.Fatalf("count after b slides out = %d, want 1 (a only)", got)
	}
	if got := w.CountRecent(testIP, now+1_000+win, win); got != 0 {
		t.Fatalf("count after a slides out = %d, want 0", got)
	}
}

func TestIPAccountWindow_ActivityKeepsSlot(t *testing.T) {
	// An account that keeps being served keeps occupying its slot: refreshing
	// activity within the window means the entry never slides out.
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	w.Touch(testIP, "a", now)
	for at := now + win/2; ; at += win/2 {
		if at-now >= 10*win {
			break
		}
		w.Touch(testIP, "a", at) // activity every win/2, forever in-window
		if got := w.CountRecent(testIP, at, win); got != 1 {
			t.Fatalf("count at=%d = %d, want 1", at, got)
		}
	}
}

func TestIPAccountWindow_RefreshOnHit(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	// Existing entry: refresh keeps the slot and returns true.
	w.Touch(testIP, "a", now)
	if !w.RefreshOnHit(testIP, "a", now+10, win, 2) {
		t.Fatal("refresh of existing entry must succeed")
	}
	if got := w.CountRecent(testIP, now+10, win); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}

	// Returning account (entry slid out), count below max: regular re-admit.
	w2 := NewIPAccountWindow()
	w2.Touch(testIP, "b", now)
	w2.Touch(testIP, "c", now)
	w2.Touch(testIP, "d", now) // 3 entries, max=3
	if !w2.RefreshOnHit(testIP, "b", now+win+1, win, 3) {
		t.Fatal("returning account below max must be re-admitted")
	}
	if got := w2.CountRecent(testIP, now+win+1, win); got != 1 {
		t.Fatalf("count = %d, want 1 (only b re-admitted)", got)
	}

	// Returning account at max: occupies the max+1 fallback slot.
	w3 := NewIPAccountWindow()
	for _, acc := range []string{"x", "y", "z"} {
		w3.Touch(testIP, acc, now)
	}
	if !w3.RefreshOnHit(testIP, "r", now+1, win, 3) {
		t.Fatal("returning account below ceiling must be re-admitted via fallback")
	}
	_, fallback := w3.Stats()
	if fallback != 1 {
		t.Fatalf("fallbackTotal = %d, want 1", fallback)
	}

	// Returning account at the max+1 ceiling: rejected, lease must be rebuilt.
	w4 := NewIPAccountWindow()
	for _, acc := range []string{"x", "y", "z", "r"} {
		w4.Touch(testIP, acc, now)
	}
	if w4.RefreshOnHit(testIP, "q", now+1, win, 3) {
		t.Fatal("admission beyond max+1 must fail")
	}
	blocked, _ := w4.Stats()
	if blocked != 1 {
		t.Fatalf("blockedTotal = %d, want 1", blocked)
	}

	// Quota disabled: always admitted.
	w5 := NewIPAccountWindow()
	if !w5.RefreshOnHit(testIP, "a", now, win, 0) {
		t.Fatal("quota disabled must always admit")
	}
}

func TestIPAccountWindow_AccountsByIPDetail(t *testing.T) {
	w := NewIPAccountWindow()
	now := int64(1_000_000_000)
	win := int64(2 * 1e9)

	w.Touch(testIP, "a", now)
	w.Touch(testIP, "b", now+5)
	// Fallback entry carries the viaFallback flag.
	w.ReserveForcedFallback(testIP, "c", now+6, win, 2)
	w.Touch(testIP, "stale", now-10*win) // already out of window

	detail := w.AccountsByIP(now+7, win)
	accounts, ok := detail[testIP]
	if !ok {
		t.Fatal("missing detail for testIP")
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %v, want 3 in-window entries", accounts)
	}
	if accounts[0].Account != "a" || accounts[1].Account != "b" || accounts[2].Account != "c" {
		t.Fatalf("accounts not sorted: %v", accounts)
	}
	if accounts[0].LastSeenNs != now || accounts[1].LastSeenNs != now+5 {
		t.Fatalf("lastSeen mismatch: %v", accounts)
	}
	if !accounts[2].ViaFallback || accounts[0].ViaFallback {
		t.Fatalf("viaFallback mismatch: %v", accounts)
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
