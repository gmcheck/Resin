package outbound

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
)

// Global upstream proxy tags used by the env-layer (RESIN_UPSTREAM_PROXY).
// Per-subscription upstream uses caller-supplied tags instead.
const (
	// globalUpstreamProxyTag is the sing-box outbound tag for the env-layer
	// global upstream proxy.
	globalUpstreamProxyTag = "__resin_upstream_proxy"
	// globalUpstreamProxyDNSTag is a dedicated "local" DNS transport used to
	// resolve the global upstream proxy server hostname via the system resolver.
	globalUpstreamProxyDNSTag = "resin-upstream-proxy-local-dns"
)

// upstreamProxySpec is a parsed upstream proxy URI ready to be turned into a
// sing-box outbound.
type upstreamProxySpec struct {
	outboundType   string // "socks" or "http"
	options        any    // *option.SOCKSOutboundOptions or *option.HTTPOutboundOptions
	serverIsDomain bool
}

// parseUpstreamProxyURL parses a proxy URI of the form
//
//	socks5://[user:pass@]host:port
//	socks5h://[user:pass@]host:port
//	http://[user:pass@]host:port
//	https://[user:pass@]host:port
//	host:port (scheme-less, treated as http)
//
// Default ports: socks 1080, http 80, https 443.
//
// ValidateUpstreamProxyURL validates an upstream proxy URL string
// (same rules as parseUpstreamProxyURL) without constructing an outbound.
func ValidateUpstreamProxyURL(raw string) error {
	_, err := parseUpstreamProxyURL(raw)
	return err
}

// redactURLCredentials removes the password from any scheme://user:pass@host
// occurrence inside s. Used only for error text: the configured value itself is
// intentionally shown verbatim in the UI.
func redactURLCredentials(s string) string {
	const marker = "://"
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i+len(marker)])
		rest = rest[i+len(marker):]
		at := strings.Index(rest, "@")
		slash := strings.IndexAny(rest, "/?# ")
		if at < 0 || (slash >= 0 && slash < at) {
			continue // no userinfo in this authority
		}
		userinfo := rest[:at]
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			userinfo = userinfo[:colon] + ":***"
		}
		b.WriteString(userinfo)
		rest = rest[at:]
	}
}

func parseUpstreamProxyURL(raw string) (upstreamProxySpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return upstreamProxySpec{}, fmt.Errorf("empty upstream proxy URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse's error string embeds the full URL, which would echo the
		// proxy password into last_error (and from there into the UI).
		return upstreamProxySpec{}, fmt.Errorf("parse upstream proxy URL: %s", redactURLCredentials(err.Error()))
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "socks", "socks5", "socks5h", "http", "https":
	default:
		return upstreamProxySpec{}, fmt.Errorf(
			"unsupported upstream proxy scheme %q (supported: socks5://, http://, https://)",
			scheme,
		)
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return upstreamProxySpec{}, fmt.Errorf("upstream proxy URL: missing host")
	}
	port, err := upstreamProxyPort(u, scheme)
	if err != nil {
		return upstreamProxySpec{}, err
	}
	_, parseErr := netip.ParseAddr(host)
	hostIsIP := parseErr == nil

	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	serverOptions := option.ServerOptions{
		Server:     host,
		ServerPort: uint16(port),
	}
	switch scheme {
	case "socks", "socks5", "socks5h":
		return upstreamProxySpec{
			outboundType:   C.TypeSOCKS,
			serverIsDomain: !hostIsIP,
			options: &option.SOCKSOutboundOptions{
				ServerOptions: serverOptions,
				Version:       "5",
				Username:      username,
				Password:      password,
			},
		}, nil
	default: // http, https
		opts := &option.HTTPOutboundOptions{
			ServerOptions: serverOptions,
			Username:      username,
			Password:      password,
		}
		if scheme == "https" {
			opts.OutboundTLSOptionsContainer.TLS = &option.OutboundTLSOptions{
				ServerName: host,
			}
		}
		return upstreamProxySpec{
			outboundType:   C.TypeHTTP,
			serverIsDomain: !hostIsIP,
			options:        opts,
		}, nil
	}
}

func upstreamProxyPort(u *url.URL, scheme string) (int, error) {
	if rawPort := u.Port(); rawPort != "" {
		p, err := strconv.Atoi(rawPort)
		if err != nil || p < 1 || p > 65535 {
			return 0, fmt.Errorf("upstream proxy URL: invalid port %q", rawPort)
		}
		return p, nil
	}
	switch scheme {
	case "socks", "socks5", "socks5h":
		return 1080, nil
	case "https":
		return 443, nil
	default:
		return 80, nil
	}
}

// createUpstreamProxy builds a dedicated upstream outbound and registers it in
// the given outbound manager so that node outbounds can detour through it.
//
// outboundTag is the sing-box tag for the registered outbound; dnsTransportTag
// is the tag for the local DNS transport used when the proxy server is a domain
// name. Callers should use unique tags per upstream instance.
//
// When the proxy server is a domain name, a dedicated system-resolver DNS
// transport is registered so the hostname resolves inside the container.
func createUpstreamProxy(
	ctx context.Context,
	outboundMgr adapter.OutboundManager,
	dnsTransportMgr adapter.DNSTransportManager,
	logger log.ContextLogger,
	outboundTag string,
	dnsTransportTag string,
	raw string,
) (adapter.Outbound, error) {
	spec, err := parseUpstreamProxyURL(raw)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}

	if spec.serverIsDomain {
		if err := dnsTransportMgr.Create(
			ctx,
			logger,
			dnsTransportTag,
			C.DNSTypeLocal,
			&option.LocalDNSServerOptions{},
		); err != nil {
			return nil, fmt.Errorf("upstream proxy: register local DNS resolver: %w", err)
		}
		setUpstreamProxyDomainResolver(spec.options, dnsTransportTag)
	}

	if err := outboundMgr.Create(
		ctx,
		nil, // router - dialing does not require one
		logger,
		outboundTag,
		spec.outboundType,
		spec.options,
	); err != nil {
		return nil, fmt.Errorf("upstream proxy: create outbound: %w", err)
	}

	ob, loaded := outboundMgr.Outbound(outboundTag)
	if !loaded || ob == nil {
		return nil, fmt.Errorf("upstream proxy: registered outbound %q not found", outboundTag)
	}

	// The outbound manager is never started in this service graph, so run the
	// lifecycle stages manually, matching SingboxBuilder.Build.
	for _, stage := range adapter.ListStartStages {
		if err := adapter.LegacyStart(ob, stage); err != nil {
			_ = common.Close(ob)
			return nil, fmt.Errorf("upstream proxy: start %s: %w", stage, err)
		}
	}

	return ob, nil
}

// setUpstreamProxyDomainResolver sets DialerOptions.DomainResolver on the
// upstream outbound options so that the proxy server hostname is resolved
// through the given DNS transport (typically a local/system resolver).
func setUpstreamProxyDomainResolver(options any, resolverTag string) {
	switch opts := options.(type) {
	case *option.SOCKSOutboundOptions:
		opts.DialerOptions.DomainResolver = &option.DomainResolveOptions{Server: resolverTag}
	case *option.HTTPOutboundOptions:
		opts.DialerOptions.DomainResolver = &option.DomainResolveOptions{Server: resolverTag}
	}
}

// injectUpstreamProxyDetour rewrites an outbound's dialer options so that its
// server connections go through the upstream proxy. An existing detour on the
// node is preserved (node-level detours take precedence).
//
// This is used by the env-layer (RESIN_UPSTREAM_PROXY) injection path in
// SingboxBuilder.Build. Per-subscription injection uses map-level detour
// writing in the parser instead.
func injectUpstreamProxyDetour(options any) {
	wrapper, ok := options.(option.DialerOptionsWrapper)
	if !ok {
		return
	}
	dialOptions := wrapper.TakeDialerOptions()
	if dialOptions.Detour != "" {
		return
	}
	dialOptions.Detour = globalUpstreamProxyTag
	wrapper.ReplaceDialerOptions(dialOptions)
}
