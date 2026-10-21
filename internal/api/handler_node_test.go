package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/probe"
	"github.com/Resinat/Resin/internal/service"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func addNodeForNodeListTest(t *testing.T, cp *service.ControlPlaneService, sub *subscription.Subscription, raw string, egressIP string) {
	addNodeForNodeListTestWithTag(t, cp, sub, raw, egressIP, "tag")
}

func addNodeForNodeListTestWithTag(
	t *testing.T,
	cp *service.ControlPlaneService,
	sub *subscription.Subscription,
	raw string,
	egressIP string,
	tag string,
) {
	t.Helper()

	hash := node.HashFromRawOptions([]byte(raw))
	cp.Pool.AddNodeFromSub(hash, []byte(raw), sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{tag}})

	if egressIP == "" {
		return
	}
	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	entry.SetEgressIP(netip.MustParseAddr(egressIP))
}

func markNodeHealthyForNodeListTest(t *testing.T, cp *service.ControlPlaneService, raw string) {
	t.Helper()

	hash := node.HashFromRawOptions([]byte(raw))
	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	entry.CircuitOpenSince.Store(0)
}

func TestHandleListNodes_TagKeywordFiltersByNodeName(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(subA)

	addNodeForNodeListTestWithTag(t, cp, subA, `{"type":"ss","server":"1.1.1.1","port":443}`, "", "hongkong-fast-01")
	addNodeForNodeListTestWithTag(t, cp, subA, `{"type":"ss","server":"2.2.2.2","port":443}`, "", "japan-slow-01")

	rec := doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+subA.ID+"&tag_keyword=FAST",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes with tag_keyword status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["total"] != float64(1) {
		t.Fatalf("tag_keyword total: got %v, want 1", body["total"])
	}
}

func TestHandleListNodes_UniqueEgressIPsUsesFilteredResult(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	subB := subscription.NewSubscription("22222222-2222-2222-2222-222222222222", "sub-b", "https://example.com/b", true, false)
	cp.SubMgr.Register(subA)
	cp.SubMgr.Register(subB)

	const rawA1 = `{"type":"ss","server":"1.1.1.1","port":443}`
	const rawA2 = `{"type":"ss","server":"2.2.2.2","port":443}`
	const rawA3 = `{"type":"ss","server":"3.3.3.3","port":443}`
	const rawA4 = `{"type":"ss","server":"4.4.4.4","port":443}`
	const rawB1 = `{"type":"ss","server":"5.5.5.5","port":443}`

	addNodeForNodeListTest(t, cp, subA, rawA1, "203.0.113.10")
	addNodeForNodeListTest(t, cp, subA, rawA2, "203.0.113.10")
	addNodeForNodeListTest(t, cp, subA, rawA3, "203.0.113.11")
	addNodeForNodeListTest(t, cp, subA, rawA4, "")
	addNodeForNodeListTest(t, cp, subB, rawB1, "203.0.113.99")

	// Healthy condition: has outbound + not circuit-open.
	markNodeHealthyForNodeListTest(t, cp, rawA1)
	markNodeHealthyForNodeListTest(t, cp, rawA2)

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?subscription_id="+subA.ID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["total"] != float64(4) {
		t.Fatalf("total: got %v, want 4", body["total"])
	}
	if body["unique_egress_ips"] != float64(2) {
		t.Fatalf("unique_egress_ips: got %v, want 2", body["unique_egress_ips"])
	}
	if body["unique_healthy_egress_ips"] != float64(1) {
		t.Fatalf("unique_healthy_egress_ips: got %v, want 1", body["unique_healthy_egress_ips"])
	}

	rec = doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+subA.ID+"&limit=1",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes paged status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["total"] != float64(4) {
		t.Fatalf("paged total: got %v, want 4", body["total"])
	}
	if body["unique_egress_ips"] != float64(2) {
		t.Fatalf("paged unique_egress_ips: got %v, want 2", body["unique_egress_ips"])
	}
	if body["unique_healthy_egress_ips"] != float64(1) {
		t.Fatalf("paged unique_healthy_egress_ips: got %v, want 1", body["unique_healthy_egress_ips"])
	}

	rec = doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+subA.ID+"&egress_ip=203.0.113.10",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes with egress filter status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["total"] != float64(2) {
		t.Fatalf("filtered total: got %v, want 2", body["total"])
	}
	if body["unique_egress_ips"] != float64(1) {
		t.Fatalf("filtered unique_egress_ips: got %v, want 1", body["unique_egress_ips"])
	}
	if body["unique_healthy_egress_ips"] != float64(1) {
		t.Fatalf("filtered unique_healthy_egress_ips: got %v, want 1", body["unique_healthy_egress_ips"])
	}
}

func TestHandleListNodes_IncludesReferenceLatencyMs(t *testing.T) {
	srv, cp, runtimeCfg := newControlPlaneTestServer(t)

	cfg := config.NewDefaultRuntimeConfig()
	cfg.LatencyAuthorities = []string{"cloudflare.com", "github.com", "google.com"}
	runtimeCfg.Store(cfg)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(subA)

	raw := `{"type":"ss","server":"1.1.1.1","port":443}`
	hash := node.HashFromRawOptions([]byte(raw))
	addNodeForNodeListTest(t, cp, subA, raw, "203.0.113.10")

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	entry.LatencyTable.LoadEntry("cloudflare.com", node.DomainLatencyStats{
		Ewma:        40 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	entry.LatencyTable.LoadEntry("github.com", node.DomainLatencyStats{
		Ewma:        80 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	entry.LatencyTable.LoadEntry("example.com", node.DomainLatencyStats{
		Ewma:        10 * time.Millisecond,
		LastUpdated: time.Now(),
	})

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?subscription_id="+subA.ID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := decodeJSONMap(t, rec)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items mismatch: got %T len=%d", body["items"], len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item type: got %T", items[0])
	}
	if item["reference_latency_ms"] != float64(60) {
		t.Fatalf("reference_latency_ms: got %v, want 60", item["reference_latency_ms"])
	}
}

func TestHandleProbeEgress_ReturnsRegion(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	raw := []byte(`{"type":"ss","server":"1.1.1.1","port":443}`)
	hash := node.HashFromRawOptions(raw)
	cp.Pool.AddNodeFromSub(hash, raw, sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)

	cp.ProbeMgr = probe.NewProbeManager(probe.ProbeConfig{
		Pool: cp.Pool,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			return []byte("ip=203.0.113.88\nloc=JP"), 25 * time.Millisecond, nil
		},
	})

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/"+hash.Hex()+"/actions/probe-egress", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe-egress status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["egress_ip"] != "203.0.113.88" {
		t.Fatalf("egress_ip: got %v, want %q", body["egress_ip"], "203.0.113.88")
	}
	if body["region"] != "jp" {
		t.Fatalf("region: got %v, want %q", body["region"], "jp")
	}
}

func TestHandleListNodes_EnabledFilter(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subEnabled := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-enabled", "https://example.com/a", true, false)
	subDisabled := subscription.NewSubscription("22222222-2222-2222-2222-222222222222", "sub-disabled", "https://example.com/b", false, false)
	cp.SubMgr.Register(subEnabled)
	cp.SubMgr.Register(subDisabled)

	addNodeForNodeListTestWithTag(t, cp, subEnabled, `{"type":"ss","server":"1.1.1.1","port":443}`, "", "enabled-tag")
	addNodeForNodeListTestWithTag(t, cp, subDisabled, `{"type":"ss","server":"2.2.2.2","port":443}`, "", "disabled-tag")

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?enabled=true", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled=true status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["total"] != float64(1) {
		t.Fatalf("enabled=true total: got %v, want 1", body["total"])
	}

	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?enabled=false", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled=false status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["total"] != float64(1) {
		t.Fatalf("enabled=false total: got %v, want 1", body["total"])
	}
}

func TestSortNodeSummaries_ReferenceLatencyOrdersEmptyValuesLast(t *testing.T) {
	latency := func(v float64) *float64 { return &v }
	circuitOpenSince := "2026-10-21T00:00:00Z"

	nodes := []service.NodeSummary{
		{NodeHash: "hash-empty", Enabled: true, HasOutbound: true},
		{NodeHash: "hash-slow", Enabled: true, HasOutbound: true, ReferenceLatencyMs: latency(300)},
		{NodeHash: "hash-fast", Enabled: true, HasOutbound: true, ReferenceLatencyMs: latency(100)},
		// Not healthy: the WebUI displays "-", so it must not take part in the
		// numeric comparison even though a latency value exists.
		{NodeHash: "hash-circuit", Enabled: true, HasOutbound: true, CircuitOpenSince: &circuitOpenSince, ReferenceLatencyMs: latency(50)},
	}

	assertOrder := func(t *testing.T, order string, want []string) {
		t.Helper()
		items := append([]service.NodeSummary(nil), nodes...)
		sortNodeSummaries(items, Sorting{SortBy: "reference_latency_ms", SortOrder: order})
		for i, expected := range want {
			if items[i].NodeHash != expected {
				got := make([]string, len(items))
				for j, item := range items {
					got[j] = item.NodeHash
				}
				t.Fatalf("sort_order=%s: got %v, want %v", order, got, want)
			}
		}
	}

	assertOrder(t, "asc", []string{"hash-fast", "hash-slow", "hash-circuit", "hash-empty"})
	assertOrder(t, "desc", []string{"hash-slow", "hash-fast", "hash-circuit", "hash-empty"})
}

func TestSortNodeSummaries_LastLatencyProbeComparesInstants(t *testing.T) {
	nodes := []service.NodeSummary{
		{NodeHash: "hash-never"},
		{NodeHash: "hash-whole-second", LastLatencyProbeAttempt: "2026-10-21T12:00:00Z"},
		{NodeHash: "hash-half-second", LastLatencyProbeAttempt: "2026-10-21T12:00:00.5Z"},
		{NodeHash: "hash-earlier", LastLatencyProbeAttempt: "2026-10-21T11:00:00Z"},
	}

	assertOrder := func(t *testing.T, order string, want []string) {
		t.Helper()
		items := append([]service.NodeSummary(nil), nodes...)
		sortNodeSummaries(items, Sorting{SortBy: "last_latency_probe_attempt", SortOrder: order})
		for i, expected := range want {
			if items[i].NodeHash != expected {
				got := make([]string, len(items))
				for j, item := range items {
					got[j] = item.NodeHash
				}
				t.Fatalf("sort_order=%s: got %v, want %v", order, got, want)
			}
		}
	}

	// "...:00Z" is earlier than "...:00.5Z" even though it sorts after it as text.
	assertOrder(t, "asc", []string{"hash-earlier", "hash-whole-second", "hash-half-second", "hash-never"})
	assertOrder(t, "desc", []string{"hash-half-second", "hash-whole-second", "hash-earlier", "hash-never"})
}

func TestHandleListNodes_AcceptsLatencySortFields(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(subA)
	addNodeForNodeListTest(t, cp, subA, `{"type":"ss","server":"1.1.1.1","port":443}`, "203.0.113.10")

	for _, field := range []string{"reference_latency_ms", "last_latency_probe_attempt"} {
		for _, order := range []string{"asc", "desc"} {
			path := "/api/v1/nodes?subscription_id=" + subA.ID + "&sort_by=" + field + "&sort_order=" + order
			rec := doJSONRequest(t, srv, http.MethodGet, path, nil, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("sort_by=%s sort_order=%s status: got %d, want %d, body=%s", field, order, rec.Code, http.StatusOK, rec.Body.String())
			}
			body := decodeJSONMap(t, rec)
			items, ok := body["items"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("sort_by=%s sort_order=%s items: got %T len=%d, want 1", field, order, body["items"], len(items))
			}
		}
	}
}
