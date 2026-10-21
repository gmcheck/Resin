package subscription

import "strings"

const (
	// upstreamPlatformPrefix marks an upstream proxy that is resolved from a
	// platform's routable node set ("platform:<platform name>") instead of a
	// fixed URI or a fixed node reference.
	upstreamPlatformPrefix = "platform:"

	// maxUpstreamPlatformNameLen bounds the platform name accepted in an
	// upstream value so a malformed config cannot smuggle a huge string into
	// error messages and logs.
	maxUpstreamPlatformNameLen = 128
)

// IsUpstreamPlatform reports whether the upstream value selects a platform as
// the upstream source ("platform:<name>").
func IsUpstreamPlatform(upstream string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(upstream)), upstreamPlatformPrefix)
}

// ParsePlatformName extracts the platform name from a "platform:<name>" value.
// It returns an empty string when the value is not a platform selector or the
// name is empty/over-long. The name is trimmed but case-preserved, matching
// platform lookup which is exact.
func ParsePlatformName(upstream string) string {
	trimmed := strings.TrimSpace(upstream)
	if !strings.HasPrefix(strings.ToLower(trimmed), upstreamPlatformPrefix) {
		return ""
	}
	name := strings.TrimSpace(trimmed[len(upstreamPlatformPrefix):])
	if name == "" || len(name) > maxUpstreamPlatformNameLen {
		return ""
	}
	return name
}

// UpstreamPlatformValue builds a normalized "platform:<name>" value.
func UpstreamPlatformValue(name string) string {
	return upstreamPlatformPrefix + strings.TrimSpace(name)
}
