package api

import (
	"net/http"
	"testing"
)

func mustCreateSub(t *testing.T, srv *Server, name, upstream string) string {
	t.Helper()
	payload := map[string]any{
		"name": name,
		"url":  "https://example.com/" + name,
	}
	if upstream != "" {
		payload["upstream_proxy"] = upstream
	}
	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", payload, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create subscription status: got %d, want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create subscription missing id: body=%s", rec.Body.String())
	}
	return id
}


func TestAPIContract_SubscriptionUpstreamProxy_CreateValid(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":            "upstream-uri",
		"url":             "https://example.com/up-uri",
		"upstream_proxy": "socks5://user:pass@127.0.0.1:1080",
	}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with uri status: got %d, want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["upstream_proxy"] != "socks5://user:pass@127.0.0.1:1080" {
		t.Fatalf("upstream_proxy roundtrip: got %v", body["upstream_proxy"])
	}

	const refHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":            "upstream-ref",
		"url":             "https://example.com/up-ref",
		"upstream_proxy": "ref:" + refHash,
	}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with ref status: got %d, want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["upstream_proxy"] != "ref:"+refHash {
		t.Fatalf("ref roundtrip: got %v", body["upstream_proxy"])
	}
}


func TestAPIContract_SubscriptionUpstreamProxy_CreateInvalid(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	for _, tc := range []struct{ name, upstream string }{
		{"bad-scheme", "ftp://1.2.3.4:9"},
		{"bad-ref", "ref:xyz"},
		{"bad-ref-len", "ref:12345"},
	} {
		rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
			"name":            tc.name,
			"url":             "https://example.com/" + tc.name,
			"upstream_proxy": tc.upstream,
		}, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("create %s status: got %d, want %d, body=%s", tc.name, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		assertErrorCode(t, rec, "INVALID_ARGUMENT")
	}
}


func TestAPIContract_SubscriptionUpstreamProxy_PatchValidated(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	id := mustCreateSub(t, srv, "patch-up", "")

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions/"+id, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["upstream_proxy"] != "" {
		t.Fatalf("default upstream_proxy should be empty: got %v", body["upstream_proxy"])
	}

	rec = doJSONRequest(t, srv, http.MethodPatch, "/api/v1/subscriptions/"+id, map[string]any{
		"upstream_proxy": "http://1.2.3.4:8080",
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["upstream_proxy"] != "http://1.2.3.4:8080" {
		t.Fatalf("patch roundtrip: got %v", body["upstream_proxy"])
	}

	rec = doJSONRequest(t, srv, http.MethodPatch, "/api/v1/subscriptions/"+id, map[string]any{
		"upstream_proxy": "ref:nothex",
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch invalid status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")
}


func TestAPIContract_SubscriptionUpstreamProxy_ListField(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	idWith := mustCreateSub(t, srv, "list-with-up", "socks5://127.0.0.1:1080")
	idWithout := mustCreateSub(t, srv, "list-no-up", "")

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items type: got %T", body["items"])
	}
	want := map[string]string{idWith: "socks5://127.0.0.1:1080", idWithout: ""}
	for _, raw := range items {
		item := raw.(map[string]any)
		id, _ := item["id"].(string)
		w, ok := want[id]
		if !ok {
			continue
		}
		if item["upstream_proxy"] != w {
			t.Fatalf("list upstream_proxy for %s: got %v, want %q", id, item["upstream_proxy"], w)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("list did not include created subscriptions: %v", want)
	}
}
