package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TunnelRoute defines a set of Cloudflare Tunnel routing rules
// without requiring a Kubernetes Ingress resource.
type TunnelRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TunnelRouteSpec   `json:"spec,omitempty"`
	Status TunnelRouteStatus `json:"status,omitempty"`
}

// TunnelRouteList contains a list of TunnelRoute.
type TunnelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelRoute `json:"items"`
}

// TunnelRouteSpec defines the desired routing rules.
type TunnelRouteSpec struct {
	Rules []TunnelRouteRule `json:"rules"`
}

// TunnelRouteRule is a single hostname-to-service routing rule.
type TunnelRouteRule struct {
	Hostname      string                    `json:"hostname"`
	Service       string                    `json:"service"`
	OriginRequest *TunnelRouteOriginRequest `json:"originRequest,omitempty"`
}

// TunnelRouteOriginRequest holds per-rule connection settings between
// cloudflared and the origin. Zero values mean "not set".
type TunnelRouteOriginRequest struct {
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
	OriginServerName string `json:"originServerName,omitempty"`
	HTTPHostHeader   string `json:"httpHostHeader,omitempty"`
	HTTP2Origin      bool   `json:"http2Origin,omitempty"`
	CAPool           string `json:"caPool,omitempty"`
}

// TunnelRouteStatus is reserved for future use.
type TunnelRouteStatus struct{}
