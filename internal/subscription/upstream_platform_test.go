package subscription

import (
	"strings"
	"testing"
)

func TestIsUpstreamPlatform(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"platform:HK", true},
		{"platform: HK ", true},
		{"  platform:HK  ", true},
		{"PLATFORM:HK", true},
		{"platform:", true}, // prefix matches; name validation happens in ParsePlatformName
		{"platformX:HK", false},
		{"socks5://127.0.0.1:1080", false},
		{"ref:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsUpstreamPlatform(c.in); got != c.want {
			t.Errorf("IsUpstreamPlatform(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParsePlatformName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"platform:HK", "HK"},
		{"platform: HK ", "HK"},
		{"  platform:Hong Kong  ", "Hong Kong"},
		{"PLATFORM:HK", "HK"},
		{"platform:", ""},
		{"platform:   ", ""},
		{"platform:" + strings.Repeat("a", 129), ""},
		{"platform:" + strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{"socks5://127.0.0.1:1080", ""},
		{"ref:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ParsePlatformName(c.in); got != c.want {
			t.Errorf("ParsePlatformName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUpstreamPlatformValue(t *testing.T) {
	if got := UpstreamPlatformValue("  HK "); got != "platform:HK" {
		t.Fatalf("UpstreamPlatformValue = %q, want %q", got, "platform:HK")
	}
}

// Platform selectors must not be mistaken for node refs or proxy URIs by the
// existing classifiers, otherwise ApplyUpstreamDetour would inject a bogus tag.
func TestUpstreamPlatform_NotClassifiedAsRefOrURI(t *testing.T) {
	const value = "platform:HK"
	if IsUpstreamRef(value) {
		t.Fatal("platform selector must not be treated as a node ref")
	}
	if ParseUpstreamRefHash(value) != "" {
		t.Fatal("platform selector must not yield a ref hash")
	}
	if ParsePlatformName(value) != "HK" {
		t.Fatal("platform selector must be parsed as a platform")
	}
}

func TestUpstreamResolvedHash_Lifecycle(t *testing.T) {
	sub := NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)

	if got := sub.UpstreamResolvedHash(); got != "" {
		t.Fatalf("initial resolved hash: got %q, want empty", got)
	}

	sub.SetUpstreamProxy("platform:HK")
	sub.SetUpstreamResolvedHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if got := sub.UpstreamResolvedHash(); got != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("resolved hash: got %q", got)
	}

	// Changing the upstream invalidates the previous pick.
	sub.SetUpstreamProxy("platform:US")
	if got := sub.UpstreamResolvedHash(); got != "" {
		t.Fatalf("resolved hash after upstream change: got %q, want empty", got)
	}

	// Re-setting the same value keeps the pick (no configVersion bump).
	sub.SetUpstreamResolvedHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	before := sub.ConfigVersion()
	sub.SetUpstreamProxy("platform:US")
	if got := sub.UpstreamResolvedHash(); got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("resolved hash should survive a no-op upstream set: got %q", got)
	}
	if sub.ConfigVersion() != before {
		t.Fatalf("no-op upstream set bumped configVersion: %d -> %d", before, sub.ConfigVersion())
	}
}
