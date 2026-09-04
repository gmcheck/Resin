package subscription

import (
	"encoding/json"
	"strings"
)

const (
	// subscriptionUpstreamTagPrefix is the tag prefix for subscription-level
	// upstream proxy outbounds registered in the sing-box outbound manager.
	subscriptionUpstreamTagPrefix = "__resin_sub_upstream_"
)

// ApplyUpstreamDetour injects a detour field into each ParsedNode's RawOptions
// so that the node outbound dials through the subscription's upstream proxy.
//
// Behavior:
//   - upstream is empty → nodes returned unchanged.
//   - upstream starts with "ref:" → detourTag = the referenced node hash hex
//     (the node outbound is registered in the sbOutbound manager with that tag).
//   - otherwise (URI) → detourTag = "__resin_sub_upstream_<subID>".
//
// Node-level detours (already present in the outbound JSON) take precedence
// and are never overwritten.
func ApplyUpstreamDetour(nodes []ParsedNode, upstream string, subID string) []ParsedNode {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return nodes
	}

	detourTag := resolveDetourTag(upstream, subID)

	result := make([]ParsedNode, len(nodes))
	for i, node := range nodes {
		raw := node.RawOptions
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			// Cannot parse → leave unchanged (will fail at build time anyway).
			result[i] = node
			continue
		}
		// Node-level detour takes precedence.
		if _, exists := m["detour"]; exists {
			result[i] = node
			continue
		}
		m["detour"] = detourTag
		injected, err := json.Marshal(m)
		if err != nil {
			result[i] = node
			continue
		}
		result[i] = ParsedNode{
			Tag:        node.Tag,
			RawOptions: injected,
		}
	}
	return result
}

// resolveDetourTag returns the detour tag for the given upstream value.
func resolveDetourTag(upstream string, subID string) string {
	upstream = strings.TrimSpace(upstream)
	if strings.HasPrefix(upstream, "ref:") {
		// ref:<hash> mode: detour tag = the referenced node's hash hex
		// (same tag used when registering the node outbound in sbOutbound manager).
		return strings.TrimPrefix(upstream, "ref:")
	}
	// URI mode: detour tag = subscription-scoped upstream outbound tag.
	return subscriptionUpstreamTagPrefix + subID
}

// IsUpstreamRef returns true if the upstream value is a node reference
// (ref:<hash> format).
func IsUpstreamRef(upstream string) bool {
	return strings.HasPrefix(strings.TrimSpace(upstream), "ref:")
}

// ParseUpstreamRefHash extracts the node hash hex from a "ref:<hash>" upstream
// value. Returns empty string if not a valid ref format.
func ParseUpstreamRefHash(upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if !strings.HasPrefix(upstream, "ref:") {
		return ""
	}
	hash := strings.TrimPrefix(upstream, "ref:")
	hash = strings.TrimSpace(hash)
	if len(hash) != 32 {
		return ""
	}
	// Basic hex validation: all characters must be valid hex digits.
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	return hash
}

// UpstreamProxyDNSTag returns the DNS transport tag for a subscription-level
// upstream proxy that uses a domain-name server.
func UpstreamProxyDNSTag(subID string) string {
	return "__resin_sub_dns_" + subID
}
