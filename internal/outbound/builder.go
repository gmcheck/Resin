package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	sJson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

// OutboundBuilder creates outbound instances from raw node options.
type OutboundBuilder interface {
	Build(rawOptions json.RawMessage) (adapter.Outbound, error)
}

// SingboxBuilderConfig configures SingboxBuilder construction.
type SingboxBuilderConfig struct {
	// DNSUpstreams configures Resin's node DNS chain.
	// Values are DNS upstream URI strings and the slice must not be empty.
	DNSUpstreams []string

	// UpstreamProxy optionally routes every node outbound through a generic
	// upstream proxy via a sing-box detour chain. Accepted forms:
	// socks5://[user:pass@]host:port, http://[user:pass@]host:port,
	// https://[user:pass@]host:port, or scheme-less host:port (http).
	// Empty string disables the global upstream proxy.
	UpstreamProxy string
}

// ---------------------------------------------------------------------------
// SingboxBuilder — creates real sing-box adapter.Outbound instances.
// ---------------------------------------------------------------------------

// SingboxBuilder builds real sing-box outbound instances from raw JSON options.
// It holds a fully-wired context with DNS services so that domain-based
// outbound servers can be resolved.
type SingboxBuilder struct {
	registry            *sbOutbound.Registry
	ctx                 context.Context
	logFactory          log.Factory
	dnsTransportManager *dns.TransportManager
	dnsRouter           *dns.Router
	outboundMgr         adapter.OutboundManager
	upstreamProxy       adapter.Outbound
	// registeredUpstreams tracks per-subscription upstream proxy outbounds
	// registered in the outbound manager. Maps outbound tag → DNS transport tag
	// (empty string if no DNS transport was needed).
	registeredUpstreams map[string]string
	// upstreamMu guards registeredUpstreams. Subscription refreshes run
	// concurrently (scheduler worker pool), so registration/unregistration can
	// happen in parallel; an unsynchronised map caused fatal
	// "concurrent map read and map write" crashes.
	upstreamMu sync.Mutex
}

// NewSingboxBuilderWithConfig creates a SingboxBuilder with a complete
// sing-box service graph (registries + DNS). The caller must call Close()
// when done.
func NewSingboxBuilderWithConfig(cfg SingboxBuilderConfig) (*SingboxBuilder, error) {
	ctx := context.Background()
	ctx = include.Context(ctx) // inject protocol registries

	logFactory := log.NewNOPFactory()
	logger := logFactory.NewLogger("resin-outbound")

	dnsRegistry, ok := service.FromContext[adapter.DNSTransportRegistry](ctx).(*dns.TransportRegistry)
	if !ok {
		return nil, fmt.Errorf("singbox builder: unexpected DNS transport registry type %T", service.FromContext[adapter.DNSTransportRegistry](ctx))
	}
	registerSecureDNSTransport(dnsRegistry)

	// --- Service graph (same order as Demos/simple-proxy/main.go) -----------

	// Endpoint Manager
	endpointMgr := endpoint.NewManager(logger, service.FromContext[adapter.EndpointRegistry](ctx))
	service.MustRegister[adapter.EndpointManager](ctx, endpointMgr)

	// Inbound Manager (required dependency even though unused)
	inboundMgr := inbound.NewManager(logger, service.FromContext[adapter.InboundRegistry](ctx), endpointMgr)
	service.MustRegister[adapter.InboundManager](ctx, inboundMgr)

	// Outbound Manager (sing-box's own manager, for detour resolution)
	outboundMgr := sbOutbound.NewManager(logger, service.FromContext[adapter.OutboundRegistry](ctx), endpointMgr, "")
	service.MustRegister[adapter.OutboundManager](ctx, outboundMgr)

	// DNS Transport Manager
	dnsTransportMgr := dns.NewTransportManager(logger, service.FromContext[adapter.DNSTransportRegistry](ctx), outboundMgr, secureDNSFailoverTransportTag)
	service.MustRegister[adapter.DNSTransportManager](ctx, dnsTransportMgr)

	// DNS Router
	dnsRouter := dns.NewRouter(ctx, logFactory, option.DNSOptions{})
	service.MustRegister[adapter.DNSRouter](ctx, dnsRouter)

	dnsTransportSpecs, err := secureDNSTransportSpecsForUpstreams(cfg.DNSUpstreams)
	if err != nil {
		return nil, fmt.Errorf("singbox builder: configure DNS transports: %w", err)
	}
	for _, spec := range dnsTransportSpecs {
		if err := dnsTransportMgr.Create(ctx, logger, spec.tag, spec.transportType, spec.options); err != nil {
			return nil, fmt.Errorf("singbox builder: create DNS transport %s[%s]: %w", spec.transportType, spec.tag, err)
		}
	}

	// Start DNS Transport Manager lifecycle
	if err := dnsTransportMgr.Start(adapter.StartStateInitialize); err != nil {
		return nil, fmt.Errorf("singbox builder: initialize DNS transport manager: %w", err)
	}
	if err := dnsTransportMgr.Start(adapter.StartStateStart); err != nil {
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: start DNS transport manager: %w", err)
	}

	// Start DNS Router lifecycle
	if err := dnsRouter.Initialize(nil); err != nil {
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: initialize DNS router: %w", err)
	}
	if err := dnsRouter.Start(adapter.StartStateStart); err != nil {
		_ = dnsRouter.Close()
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: start DNS router: %w", err)
	}

	registry := service.FromContext[adapter.OutboundRegistry](ctx).(*sbOutbound.Registry)

	builder := &SingboxBuilder{
		registry:            registry,
		ctx:                 ctx,
		logFactory:          logFactory,
		dnsTransportManager: dnsTransportMgr,
		dnsRouter:           dnsRouter,
		outboundMgr:         outboundMgr,
	}

	// Optionally route every node outbound through a generic upstream proxy
	// via a sing-box detour chain.
	if strings.TrimSpace(cfg.UpstreamProxy) != "" {
		upstream, err := createUpstreamProxy(
			ctx,
			outboundMgr,
			dnsTransportMgr,
			logger,
			globalUpstreamProxyTag,
			globalUpstreamProxyDNSTag,
			cfg.UpstreamProxy,
		)
		if err != nil {
			_ = dnsRouter.Close()
			_ = dnsTransportMgr.Close()
			return nil, err
		}
		builder.upstreamProxy = upstream
	}

	return builder, nil
}

// Build parses rawOptions (a complete sing-box outbound JSON object with
// type/tag fields) into a real adapter.Outbound and runs it through the
// lifecycle stages.
func (b *SingboxBuilder) Build(rawOptions json.RawMessage) (adapter.Outbound, error) {
	// 1. Parse via official option.Outbound path (strips type/tag, creates
	//    typed options via OutboundOptionsRegistry + badjson.UnmarshallExcluded).
	var outboundConfig option.Outbound
	if err := sJson.UnmarshalContext(b.ctx, rawOptions, &outboundConfig); err != nil {
		return nil, fmt.Errorf("parse outbound options: %w", err)
	}

	// 1b. When an upstream proxy is configured, detour every node outbound
	// through it so node connections egress via the proxy instead of the
	// container's direct route. Existing node-level detours win.
	if b.upstreamProxy != nil {
		injectUpstreamProxyDetour(outboundConfig.Options)
	}

	// 2. Create the outbound instance via the registry.
	logger := b.logFactory.NewLogger("outbound/" + outboundConfig.Type)
	ob, err := b.registry.CreateOutbound(
		b.ctx,
		nil, // router — not needed for simple dialing
		logger,
		outboundConfig.Tag,
		outboundConfig.Type,
		outboundConfig.Options,
	)
	if err != nil {
		return nil, fmt.Errorf("create outbound [%s]: %w", outboundConfig.Type, err)
	}

	// 3. Run lifecycle start stages. On failure, close and return error.
	for _, stage := range adapter.ListStartStages {
		if err := adapter.LegacyStart(ob, stage); err != nil {
			_ = common.Close(ob)
			return nil, fmt.Errorf("outbound start %s [%s]: %w", stage, outboundConfig.Type, err)
		}
	}

	return ob, nil
}

// RegisterDetourOutbound creates a per-subscription upstream proxy outbound and
// registers it in the sing-box outbound manager so that node outbounds can
// detour through it. If an outbound with the same tag already exists, it is
// unregistered first (idempotent).
//
// outboundTag is the sing-box tag for the registered outbound (e.g.
// "__resin_sub_upstream_<subID>"). dnsTag is the DNS transport tag for domain
// resolution (e.g. "__resin_sub_dns_<subID>"); if empty, a default is derived.
// raw is the upstream proxy URI that will be parsed by createUpstreamProxy.
func (b *SingboxBuilder) RegisterDetourOutbound(outboundTag string, dnsTag string, raw string) error {
	if dnsTag == "" {
		dnsTag = outboundTag + "_dns"
	}
	// Idempotent: unregister first if exists.
	b.UnregisterDetourOutbound(outboundTag)

	logger := b.logFactory.NewLogger("upstream/" + outboundTag)
	_, err := createUpstreamProxy(
		b.ctx,
		b.outboundMgr,
		b.dnsTransportManager,
		logger,
		outboundTag,
		dnsTag,
		raw,
	)
	if err != nil {
		return fmt.Errorf("register detour outbound %q: %w", outboundTag, err)
	}

	b.upstreamMu.Lock()
	if b.registeredUpstreams == nil {
		b.registeredUpstreams = make(map[string]string)
	}
	b.registeredUpstreams[outboundTag] = dnsTag
	b.upstreamMu.Unlock()
	return nil
}

// UnregisterDetourOutbound removes a per-subscription upstream proxy outbound
// from tracking and closes its DNS transport if one was registered. The outbound
// itself stays in the sing-box outbound manager (no per-outbound Remove API);
// RegisterDetourOutbound with the same tag will replace it via Create's built-in
// idempotent handling. No-op if the tag is not registered.
func (b *SingboxBuilder) UnregisterDetourOutbound(outboundTag string) {
	b.upstreamMu.Lock()
	dnsTag, exists := b.registeredUpstreams[outboundTag]
	if !exists {
		b.upstreamMu.Unlock()
		return
	}
	delete(b.registeredUpstreams, outboundTag)
	b.upstreamMu.Unlock()

	// Close the DNS transport if one was registered.
	if dnsTag != "" && b.dnsTransportManager != nil {
		_ = b.dnsTransportManager.Remove(dnsTag)
	}
}

// RegisterNodeOutbound registers a built node outbound in the sing-box outbound
// manager so that it can be used as a detour target by other node outbounds
// (ref:<hash> mode). The tag is the node's hash hex.
//
// This parses rawOptions, extracts the outbound type and typed options, then
// calls outboundMgr.Create which replaces any existing outbound with the same
// tag (idempotent).
func (b *SingboxBuilder) RegisterNodeOutbound(tag string, rawOptions json.RawMessage) error {
	var outboundConfig option.Outbound
	if err := sJson.UnmarshalContext(b.ctx, rawOptions, &outboundConfig); err != nil {
		return fmt.Errorf("parse node outbound options for ref registration: %w", err)
	}
	logger := b.logFactory.NewLogger("node/" + tag)
	return b.outboundMgr.Create(
		b.ctx,
		nil, // router — not needed for dialing
		logger,
		tag,
		outboundConfig.Type,
		outboundConfig.Options,
	)
}

// UnregisterNodeOutbound removes a node outbound from the sing-box outbound
// manager. Since the manager has no per-outbound Remove, this is a no-op; the
// outbound remains as a dead reference. RegisterNodeOutbound with the same tag
// will replace it.
func (b *SingboxBuilder) UnregisterNodeOutbound(_ string) {
	// sing-box outbound manager has no per-outbound Remove API.
	// The outbound stays as a dead reference; Create with the same tag replaces it.
}

// Close releases the global upstream outbound (if any), all per-subscription
// upstream outbounds, and shuts down the builder's internal DNS services.
func (b *SingboxBuilder) Close() error {
	var errs []error
	// Close per-subscription upstream outbounds.
	b.upstreamMu.Lock()
	tags := make([]string, 0, len(b.registeredUpstreams))
	for tag := range b.registeredUpstreams {
		tags = append(tags, tag)
	}
	b.upstreamMu.Unlock()
	for _, tag := range tags {
		b.UnregisterDetourOutbound(tag)
	}
	if b.upstreamProxy != nil {
		errs = append(errs, common.Close(b.upstreamProxy))
	}
	if b.dnsRouter != nil {
		errs = append(errs, b.dnsRouter.Close())
	}
	if b.dnsTransportManager != nil {
		errs = append(errs, b.dnsTransportManager.Close())
	}
	return errors.Join(errs...)
}
