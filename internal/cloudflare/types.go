package cloudflare

// TunnelConfig is the desired state of a remotely-managed tunnel configuration.
// It intentionally models only the fields this controller manages; anything
// else in the remote configuration is ignored (and overwritten on update,
// since the configurations API replaces the whole document).
type TunnelConfig struct {
	Ingress []IngressRule
}

// IngressRule is a single public hostname rule of the tunnel configuration.
// The last rule must be a catch-all (empty Hostname, e.g. "http_status:404").
type IngressRule struct {
	Hostname      string
	Service       string
	OriginRequest *OriginRequest
}

// OriginRequest holds the per-rule connection settings between cloudflared
// and the origin. Zero values mean "not set" (cloudflared defaults apply).
type OriginRequest struct {
	NoTLSVerify      bool
	OriginServerName string
	HTTPHostHeader   string
	HTTP2Origin      bool
	CAPool           string
}

// Equal reports whether two configurations are semantically identical.
// A nil OriginRequest and an all-zero one are considered equal, so that
// configurations round-tripped through the Cloudflare API compare equal
// to locally built ones.
func (c *TunnelConfig) Equal(other *TunnelConfig) bool {
	if other == nil {
		return false
	}
	if len(c.Ingress) != len(other.Ingress) {
		return false
	}
	for i := range c.Ingress {
		a, b := c.Ingress[i], other.Ingress[i]
		if a.Hostname != b.Hostname || a.Service != b.Service {
			return false
		}
		if derefOriginRequest(a.OriginRequest) != derefOriginRequest(b.OriginRequest) {
			return false
		}
	}
	return true
}

func derefOriginRequest(or *OriginRequest) OriginRequest {
	if or == nil {
		return OriginRequest{}
	}
	return *or
}
