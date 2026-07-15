package cloudflare

import (
	"context"
	"fmt"
	"strings"
	"sync"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	"github.com/cloudflare/cloudflare-go/v7/zones"
)

// API is the subset of Cloudflare operations the controller needs.
// It is kept deliberately small so the underlying SDK (which has frequent
// major version bumps) can be swapped without touching the reconciler.
type API interface {
	// GetTunnelConfig returns the current remotely-managed configuration
	// of the tunnel, reduced to the fields this controller manages.
	GetTunnelConfig(ctx context.Context) (*TunnelConfig, error)
	// UpdateTunnelConfig replaces the whole tunnel configuration.
	UpdateTunnelConfig(ctx context.Context, cfg *TunnelConfig) error
	// EnsureDNSRecord creates or updates the proxied CNAME record pointing
	// hostname at the tunnel. Records not created by this controller
	// (identified by the record comment) are never touched.
	EnsureDNSRecord(ctx context.Context, hostname string) error
	// DeleteDNSRecord removes the CNAME record for hostname if and only if
	// it was created by this controller.
	DeleteDNSRecord(ctx context.Context, hostname string) error
}

// Client implements API against the Cloudflare v4 API via cloudflare-go v7.
type Client struct {
	api       *cf.Client
	accountID string
	tunnelID  string
	comment   string

	mu        sync.Mutex
	zoneCache map[string]string // zone name -> zone ID
}

var _ API = (*Client)(nil)

// commentPrefix marks DNS records owned by this controller. The DNS record
// comment field (max 100 chars) is used as an ownership marker so that
// foreign records are never modified or deleted.
const commentPrefix = "managed-by:cloudflared-ingress-router"

func NewClient(apiToken, accountID, tunnelID string) *Client {
	return &Client{
		api:       cf.NewClient(option.WithAPIToken(apiToken)),
		accountID: accountID,
		tunnelID:  tunnelID,
		comment:   commentPrefix + " tunnel:" + tunnelID,
	}
}

func (c *Client) tunnelTarget() string {
	return c.tunnelID + ".cfargotunnel.com"
}

func (c *Client) GetTunnelConfig(ctx context.Context) (*TunnelConfig, error) {
	res, err := c.api.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(ctx, c.tunnelID, zero_trust.TunnelCloudflaredConfigurationGetParams{
		AccountID: cf.F(c.accountID),
	})
	if err != nil {
		return nil, fmt.Errorf("get tunnel configuration: %w", err)
	}
	cfg := &TunnelConfig{}
	for _, in := range res.Config.Ingress {
		rule := IngressRule{
			Hostname: in.Hostname,
			Service:  in.Service,
		}
		or := OriginRequest{
			NoTLSVerify:      in.OriginRequest.NoTLSVerify,
			OriginServerName: in.OriginRequest.OriginServerName,
			HTTPHostHeader:   in.OriginRequest.HTTPHostHeader,
			HTTP2Origin:      in.OriginRequest.HTTP2Origin,
			CAPool:           in.OriginRequest.CAPool,
		}
		if or != (OriginRequest{}) {
			rule.OriginRequest = &or
		}
		cfg.Ingress = append(cfg.Ingress, rule)
	}
	return cfg, nil
}

func (c *Client) UpdateTunnelConfig(ctx context.Context, cfg *TunnelConfig) error {
	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(cfg.Ingress))
	for _, rule := range cfg.Ingress {
		in := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Service: cf.F(rule.Service),
		}
		// The API requires the hostname field; the catch-all rule is
		// expressed with an empty hostname.
		in.Hostname = cf.F(rule.Hostname)
		if or := rule.OriginRequest; or != nil {
			p := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngressOriginRequest{}
			if or.NoTLSVerify {
				p.NoTLSVerify = cf.F(true)
			}
			if or.OriginServerName != "" {
				p.OriginServerName = cf.F(or.OriginServerName)
			}
			if or.HTTPHostHeader != "" {
				p.HTTPHostHeader = cf.F(or.HTTPHostHeader)
			}
			if or.HTTP2Origin {
				p.HTTP2Origin = cf.F(true)
			}
			if or.CAPool != "" {
				p.CAPool = cf.F(or.CAPool)
			}
			in.OriginRequest = cf.F(p)
		}
		rules = append(rules, in)
	}
	_, err := c.api.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, c.tunnelID, zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cf.F(c.accountID),
		Config: cf.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cf.F(rules),
		}),
	})
	if err != nil {
		return fmt.Errorf("update tunnel configuration: %w", err)
	}
	return nil
}

func (c *Client) EnsureDNSRecord(ctx context.Context, hostname string) error {
	zoneID, err := c.zoneIDForHostname(ctx, hostname)
	if err != nil {
		return err
	}
	rec, err := c.findRecord(ctx, zoneID, hostname)
	if err != nil {
		return err
	}
	body := dns.CNAMERecordParam{
		Name:    cf.F(hostname),
		Type:    cf.F(dns.CNAMERecordTypeCNAME),
		Content: cf.F(c.tunnelTarget()),
		Proxied: cf.F(true),
		TTL:     cf.F(dns.TTL(1)), // 1 = automatic
		Comment: cf.F(c.comment),
	}
	if rec == nil {
		_, err := c.api.DNS.Records.New(ctx, dns.RecordNewParams{
			ZoneID: cf.F(zoneID),
			Body:   body,
		})
		if err != nil {
			return fmt.Errorf("create DNS record for %s: %w", hostname, err)
		}
		return nil
	}
	if rec.Comment != c.comment {
		return fmt.Errorf("DNS record for %s already exists and is not managed by this controller (comment: %q)", hostname, rec.Comment)
	}
	if string(rec.Type) == "CNAME" && rec.Content == c.tunnelTarget() && rec.Proxied {
		return nil
	}
	_, err = c.api.DNS.Records.Update(ctx, rec.ID, dns.RecordUpdateParams{
		ZoneID: cf.F(zoneID),
		Body:   body,
	})
	if err != nil {
		return fmt.Errorf("update DNS record for %s: %w", hostname, err)
	}
	return nil
}

func (c *Client) DeleteDNSRecord(ctx context.Context, hostname string) error {
	zoneID, err := c.zoneIDForHostname(ctx, hostname)
	if err != nil {
		return err
	}
	rec, err := c.findRecord(ctx, zoneID, hostname)
	if err != nil {
		return err
	}
	// Absent, or present but owned by someone else: nothing to clean up.
	if rec == nil || rec.Comment != c.comment {
		return nil
	}
	_, err = c.api.DNS.Records.Delete(ctx, rec.ID, dns.RecordDeleteParams{
		ZoneID: cf.F(zoneID),
	})
	if err != nil {
		return fmt.Errorf("delete DNS record for %s: %w", hostname, err)
	}
	return nil
}

func (c *Client) findRecord(ctx context.Context, zoneID, hostname string) (*dns.RecordResponse, error) {
	page, err := c.api.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cf.F(zoneID),
		Name:   cf.F(dns.RecordListParamsName{Exact: cf.F(hostname)}),
	})
	if err != nil {
		return nil, fmt.Errorf("list DNS records for %s: %w", hostname, err)
	}
	if len(page.Result) == 0 {
		return nil, nil
	}
	return &page.Result[0], nil
}

// zoneIDForHostname resolves the zone a hostname belongs to by longest
// suffix match against the zones visible to the API token.
func (c *Client) zoneIDForHostname(ctx context.Context, hostname string) (string, error) {
	zoneCache, err := c.loadZones(ctx)
	if err != nil {
		return "", err
	}
	best := ""
	for name := range zoneCache {
		if hostname != name && !strings.HasSuffix(hostname, "."+name) {
			continue
		}
		if len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return "", fmt.Errorf("no Cloudflare zone found for hostname %s", hostname)
	}
	return zoneCache[best], nil
}

func (c *Client) loadZones(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.zoneCache != nil {
		return c.zoneCache, nil
	}
	cache := map[string]string{}
	iter := c.api.Zones.ListAutoPaging(ctx, zones.ZoneListParams{
		Account: cf.F(zones.ZoneListParamsAccount{ID: cf.F(c.accountID)}),
	})
	for iter.Next() {
		z := iter.Current()
		cache[z.Name] = z.ID
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	c.zoneCache = cache
	return cache, nil
}
