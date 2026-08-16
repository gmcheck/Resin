package service

import (
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/topology"
)

// newIPQuotaTestService builds a minimal ControlPlaneService with persistence
// (state engine + topology pool + router) for IP-quota contract tests.
func newIPQuotaTestService(t *testing.T) *ControlPlaneService {
	t.Helper()
	dir := t.TempDir()
	engine, closer, err := state.PersistenceBootstrap(
		filepath.Join(dir, "state"),
		filepath.Join(dir, "cache"),
	)
	if err != nil {
		t.Fatalf("PersistenceBootstrap: %v", err)
	}
	t.Cleanup(func() {
		_ = closer.Close()
	})

	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})
	router := routing.NewRouter(routing.RouterConfig{
		Pool:        pool,
		Authorities: func() []string { return []string{"cloudflare.com"} },
		P2CWindow:   func() time.Duration { return 10 * time.Minute },
	})

	return &ControlPlaneService{
		Engine: engine,
		Pool:   pool,
		SubMgr: subMgr,
		Router: router,
		EnvCfg: &config.EnvConfig{
			DefaultPlatformStickyTTL:              30 * time.Minute,
			DefaultPlatformRegexFilters:           []string{},
			DefaultPlatformRegionFilters:          []string{},
			DefaultPlatformReverseProxyMissAction: "TREAT_AS_EMPTY",
			DefaultPlatformAllocationPolicy:       "BALANCED",
		},
	}
}

func TestIPQuotaContract_CreateWithQuotaFields(t *testing.T) {
	cp := newIPQuotaTestService(t)

	name := "quota-platform"
	max := 3
	window := "2h"
	created, err := cp.CreatePlatform(CreatePlatformRequest{
		Name:             &name,
		MaxAccountsPerIP: &max,
		IPAccountWindow:  &window,
	})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}
	if created.MaxAccountsPerIP != 3 {
		t.Fatalf("response max_accounts_per_ip = %d, want 3", created.MaxAccountsPerIP)
	}
	if created.IPAccountWindow != "2h0m0s" {
		t.Fatalf("response ip_account_window = %q, want 2h0m0s", created.IPAccountWindow)
	}

	// Runtime platform carries the quota config and is enabled.
	plat, ok := cp.Pool.GetPlatform(created.ID)
	if !ok {
		t.Fatal("platform not registered in pool")
	}
	if plat.MaxAccountsPerIP != 3 || plat.IPAccountWindowNs != int64(2*time.Hour) {
		t.Fatalf(
			"runtime platform quota = (%d, %d), want (3, %d)",
			plat.MaxAccountsPerIP, plat.IPAccountWindowNs, int64(2*time.Hour),
		)
	}
	if !plat.IPQuotaEnabled() {
		t.Fatal("IPQuotaEnabled should be true")
	}
	if got := plat.EffectiveIPAccountWindowNs(); got != int64(2*time.Hour) {
		t.Fatalf("effective window = %d, want %d", got, int64(2*time.Hour))
	}

	// Persisted model round-trips both columns.
	persisted, err := cp.Engine.GetPlatform(created.ID)
	if err != nil {
		t.Fatalf("Engine.GetPlatform: %v", err)
	}
	if persisted.MaxAccountsPerIP != 3 || persisted.IPAccountWindowNs != int64(2*time.Hour) {
		t.Fatalf(
			"persisted quota = (%d, %d), want (3, %d)",
			persisted.MaxAccountsPerIP, persisted.IPAccountWindowNs, int64(2*time.Hour),
		)
	}
}

func TestIPQuotaContract_LegacyPayloadDefaultsToDisabled(t *testing.T) {
	cp := newIPQuotaTestService(t)

	name := "legacy-platform"
	created, err := cp.CreatePlatform(CreatePlatformRequest{Name: &name})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}
	if created.MaxAccountsPerIP != 0 || created.IPAccountWindow != "0s" {
		t.Fatalf(
			"legacy defaults = (%d, %q), want (0, 0s)",
			created.MaxAccountsPerIP, created.IPAccountWindow,
		)
	}

	plat, ok := cp.Pool.GetPlatform(created.ID)
	if !ok {
		t.Fatal("platform not registered in pool")
	}
	if plat.IPQuotaEnabled() {
		t.Fatal("quota must be disabled by default (zero-value compatibility)")
	}
	// Window falls back to the documented default when unset.
	if got := plat.EffectiveIPAccountWindowNs(); got != int64(2*time.Hour) {
		t.Fatalf("effective default window = %d, want %d", got, int64(2*time.Hour))
	}
}

func TestIPQuotaContract_CreateValidation(t *testing.T) {
	cp := newIPQuotaTestService(t)

	cases := []struct {
		name    string
		max     *int
		window  *string
		wantErr string
	}{
		{
			name:    "negative max",
			max:     intPtr(-1),
			wantErr: "max_accounts_per_ip",
		},
		{
			name:    "zero window",
			window:  strPtr("0s"),
			wantErr: "ip_account_window",
		},
		{
			name:    "negative window",
			window:  strPtr("-5m"),
			wantErr: "ip_account_window",
		},
		{
			name:    "malformed window",
			window:  strPtr("not-a-duration"),
			wantErr: "ip_account_window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "val-platform"
			_, err := cp.CreatePlatform(CreatePlatformRequest{
				Name:             &name,
				MaxAccountsPerIP: tc.max,
				IPAccountWindow:  tc.window,
			})
			if err == nil {
				t.Fatal("expected validation error")
			}
			assertServiceErrorCode(t, err, "INVALID_ARGUMENT")
		})
	}
}

func TestIPQuotaContract_PatchUpdatesAndClears(t *testing.T) {
	cp := newIPQuotaTestService(t)

	name := "patch-platform"
	max := 2
	window := "1h"
	created, err := cp.CreatePlatform(CreatePlatformRequest{
		Name:             &name,
		MaxAccountsPerIP: &max,
		IPAccountWindow:  &window,
	})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}

	// Patch max only: window must be preserved.
	patch, err := json.Marshal(map[string]any{"max_accounts_per_ip": 5})
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	updated, err := cp.UpdatePlatform(created.ID, patch)
	if err != nil {
		t.Fatalf("UpdatePlatform: %v", err)
	}
	if updated.MaxAccountsPerIP != 5 {
		t.Fatalf("patched max = %d, want 5", updated.MaxAccountsPerIP)
	}
	if updated.IPAccountWindow != "1h0m0s" {
		t.Fatalf("patched window = %q, want unchanged 1h0m0s", updated.IPAccountWindow)
	}

	// Runtime platform reflects the patch.
	plat, ok := cp.Pool.GetPlatform(created.ID)
	if !ok {
		t.Fatal("platform not registered in pool")
	}
	if plat.MaxAccountsPerIP != 5 {
		t.Fatalf("runtime max = %d, want 5", plat.MaxAccountsPerIP)
	}

	// Patch window only.
	patch, err = json.Marshal(map[string]any{"ip_account_window": "30m"})
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	if _, err = cp.UpdatePlatform(created.ID, patch); err != nil {
		t.Fatalf("UpdatePlatform window: %v", err)
	}
	plat, _ = cp.Pool.GetPlatform(created.ID)
	if plat.IPAccountWindowNs != int64(30*time.Minute) {
		t.Fatalf("runtime window = %d, want %d", plat.IPAccountWindowNs, int64(30*time.Minute))
	}

	// Setting max back to 0 disables the quota.
	patch, err = json.Marshal(map[string]any{"max_accounts_per_ip": 0})
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	if _, err = cp.UpdatePlatform(created.ID, patch); err != nil {
		t.Fatalf("UpdatePlatform disable: %v", err)
	}
	plat, _ = cp.Pool.GetPlatform(created.ID)
	if plat.IPQuotaEnabled() {
		t.Fatal("quota should be disabled after max_accounts_per_ip=0")
	}

	// Persisted state survives the disable patch.
	persisted, err := cp.Engine.GetPlatform(created.ID)
	if err != nil {
		t.Fatalf("Engine.GetPlatform: %v", err)
	}
	if persisted.MaxAccountsPerIP != 0 || persisted.IPAccountWindowNs != int64(30*time.Minute) {
		t.Fatalf(
			"persisted = (%d, %d), want (0, %d)",
			persisted.MaxAccountsPerIP, persisted.IPAccountWindowNs, int64(30*time.Minute),
		)
	}
}

func TestIPQuotaContract_PatchValidation(t *testing.T) {
	cp := newIPQuotaTestService(t)

	name := "patch-val-platform"
	created, err := cp.CreatePlatform(CreatePlatformRequest{Name: &name})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}

	cases := []struct {
		key   string
		value any
	}{
		{"max_accounts_per_ip", -3},
		{"ip_account_window", "0s"},
		{"ip_account_window", "abc"},
	}
	for _, tc := range cases {
		patch, err := json.Marshal(map[string]any{tc.key: tc.value})
		if err != nil {
			t.Fatalf("marshal patch: %v", err)
		}
		if _, err = cp.UpdatePlatform(created.ID, patch); err == nil {
			t.Fatalf("patch %s=%v should be rejected", tc.key, tc.value)
		}
		assertServiceErrorCode(t, err, "INVALID_ARGUMENT")
	}
}

func TestIPQuotaContract_GetIPQuotaEndpoint(t *testing.T) {
	cp := newIPQuotaTestService(t)

	// Unknown platform → NOT_FOUND.
	_, err := cp.GetIPQuota("no-such-platform")
	if err == nil {
		t.Fatal("expected NOT_FOUND for unknown platform")
	}
	assertServiceErrorCode(t, err, "NOT_FOUND")

	name := "quota-view-platform"
	max := 3
	window := "2h"
	created, err := cp.CreatePlatform(CreatePlatformRequest{
		Name:             &name,
		MaxAccountsPerIP: &max,
		IPAccountWindow:  &window,
	})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}

	resp, err := cp.GetIPQuota(created.ID)
	if err != nil {
		t.Fatalf("GetIPQuota: %v", err)
	}
	if !resp.Enabled || resp.MaxAccountsPerIP != 3 {
		t.Fatalf("quota response = (enabled=%v, max=%d), want (true, 3)", resp.Enabled, resp.MaxAccountsPerIP)
	}
	if resp.IPAccountWindow != "2h0m0s" {
		t.Fatalf("window = %q, want 2h0m0s", resp.IPAccountWindow)
	}
	if resp.BlockedTotal != 0 || resp.FallbackTotal != 0 {
		t.Fatalf("counters = (%d, %d), want (0, 0)", resp.BlockedTotal, resp.FallbackTotal)
	}
	if resp.IPs == nil {
		t.Fatal("IPs must be a non-nil array for JSON contract stability")
	}
}

func TestIPQuotaContract_GetIPQuotaAccountDetails(t *testing.T) {
	cp := newIPQuotaTestService(t)

	name := "quota-detail-platform"
	max := 2
	window := "2h"
	created, err := cp.CreatePlatform(CreatePlatformRequest{
		Name:             &name,
		MaxAccountsPerIP: &max,
		IPAccountWindow:  &window,
	})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}

	// Bind two accounts to one egress IP through the control plane.
	nowNs := time.Now().UnixNano()
	upsert := func(account string) {
		t.Helper()
		ml := model.Lease{
			PlatformID:  created.ID,
			Account:     account,
			NodeHash:    "00000000000000000000000000000000",
			EgressIP:    "10.9.9.9",
			CreatedAtNs: nowNs,
			ExpiryNs:    nowNs + int64(time.Hour),
		}
		if err := cp.Router.UpsertLease(ml); err != nil {
			t.Fatalf("UpsertLease %s: %v", account, err)
		}
	}
	upsert("acct-a")
	upsert("acct-b")

	// Release one lease: the window entry must remain as residual exposure.
	if !cp.Router.DeleteLease(created.ID, "acct-a") {
		t.Fatal("DeleteLease acct-a failed")
	}

	resp, err := cp.GetIPQuota(created.ID)
	if err != nil {
		t.Fatalf("GetIPQuota: %v", err)
	}
	if len(resp.IPs) != 1 || resp.IPs[0].EgressIP != "10.9.9.9" {
		t.Fatalf("IPs = %+v, want one entry for 10.9.9.9", resp.IPs)
	}
	ipEntry := resp.IPs[0]
	if ipEntry.WindowAccounts != 2 {
		t.Fatalf("window_accounts = %d, want 2 (residual entry still counted)", ipEntry.WindowAccounts)
	}
	if len(ipEntry.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want 2 details", ipEntry.Accounts)
	}
	if ipEntry.Accounts[0].Account != "acct-a" || ipEntry.Accounts[1].Account != "acct-b" {
		t.Fatalf("accounts must be sorted by name, got %+v", ipEntry.Accounts)
	}
	byAccount := make(map[string]IPQuotaAccountEntry, len(ipEntry.Accounts))
	for _, acc := range ipEntry.Accounts {
		byAccount[acc.Account] = acc
	}
	if byAccount["acct-a"].HasLease {
		t.Fatal("acct-a lease was deleted: has_lease must be false (residual)")
	}
	if !byAccount["acct-b"].HasLease {
		t.Fatal("acct-b holds a lease: has_lease must be true")
	}
	for _, acc := range byAccount {
		if acc.ViaFallback {
			t.Fatalf("%s entered via the normal path: via_fallback must be false", acc.Account)
		}
		if _, err := time.Parse(time.RFC3339, acc.LastSeen); err != nil {
			t.Fatalf("%s last_seen = %q, want RFC3339: %v", acc.Account, acc.LastSeen, err)
		}
		if acc.LastSeenNs <= 0 {
			t.Fatalf("%s last_seen_ns = %d, want > 0", acc.Account, acc.LastSeenNs)
		}
	}
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }
