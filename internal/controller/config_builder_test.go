package controller

import (
	"testing"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/windyakin/cloudflared-ingress-router/internal/cloudflare"
)

const testPrefix = "cloudflared-ingress-router.windyakin.net"

var testOpts = BuildOptions{
	AnnotationPrefix: testPrefix,
	OriginURLHTTPS:   "https://traefik.kube-system.svc.cluster.local:443",
	OriginURLHTTP:    "http://traefik.kube-system.svc.cluster.local:80",
}

func makeIngress(namespace, name string, annotations map[string]string, hosts ...string) netv1.Ingress {
	full := map[string]string{}
	for k, v := range annotations {
		full[testPrefix+"/"+k] = v
	}
	ing := netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: full},
	}
	for _, h := range hosts {
		ing.Spec.Rules = append(ing.Spec.Rules, netv1.IngressRule{Host: h})
	}
	return ing
}

func TestBuildTunnelConfigDefaults(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "app", map[string]string{"enabled": "true"}, "app.example.com"),
	}, testOpts)

	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", res.Warnings)
	}
	want := &cloudflare.TunnelConfig{Ingress: []cloudflare.IngressRule{
		{
			Hostname: "app.example.com",
			Service:  "https://traefik.kube-system.svc.cluster.local:443",
			OriginRequest: &cloudflare.OriginRequest{
				OriginServerName: "app.example.com",
			},
		},
		{Service: "http_status:404"},
	}}
	if !res.Config.Equal(want) {
		t.Errorf("config mismatch:\n got  %+v\n want %+v", res.Config, want)
	}
	if res.Hostnames["app.example.com"] == nil {
		t.Errorf("hostname owner not recorded")
	}
}

func TestBuildTunnelConfigSkipsDisabledAndSortsRules(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "b", map[string]string{"enabled": "true"}, "zzz.example.com"),
		makeIngress("default", "a", map[string]string{"enabled": "true"}, "aaa.example.com"),
		makeIngress("default", "off", nil, "off.example.com"),
		makeIngress("default", "explicit-off", map[string]string{"enabled": "false"}, "off2.example.com"),
	}, testOpts)

	var hosts []string
	for _, rule := range res.Config.Ingress {
		hosts = append(hosts, rule.Hostname)
	}
	want := []string{"aaa.example.com", "zzz.example.com", ""}
	if len(hosts) != len(want) {
		t.Fatalf("got rules for %v, want %v", hosts, want)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Errorf("rule %d hostname = %q, want %q", i, hosts[i], want[i])
		}
	}
}

func TestBuildTunnelConfigHTTPScheme(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "app", map[string]string{"enabled": "true", "origin-scheme": "http"}, "app.example.com"),
	}, testOpts)

	rule := res.Config.Ingress[0]
	if rule.Service != testOpts.OriginURLHTTP {
		t.Errorf("service = %q, want %q", rule.Service, testOpts.OriginURLHTTP)
	}
	if rule.OriginRequest != nil {
		t.Errorf("http origin should not set originRequest, got %+v", rule.OriginRequest)
	}
}

func TestBuildTunnelConfigTLSAnnotations(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "app", map[string]string{
			"enabled":            "true",
			"no-tls-verify":      "true",
			"origin-server-name": "internal.example.com",
			"http-host-header":   "app.example.com",
			"http2-origin":       "true",
			"ca-pool":            "/etc/ssl/private/ca.pem",
		}, "app.example.com"),
	}, testOpts)

	or := res.Config.Ingress[0].OriginRequest
	if or == nil {
		t.Fatal("originRequest not set")
	}
	want := cloudflare.OriginRequest{
		NoTLSVerify:      true,
		OriginServerName: "internal.example.com",
		HTTPHostHeader:   "app.example.com",
		HTTP2Origin:      true,
		CAPool:           "/etc/ssl/private/ca.pem",
	}
	if *or != want {
		t.Errorf("originRequest = %+v, want %+v", *or, want)
	}
}

func TestBuildTunnelConfigOriginServiceOverride(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "app", map[string]string{
			"enabled":        "true",
			"origin-service": "http://direct.default.svc.cluster.local:8080",
		}, "app.example.com"),
	}, testOpts)

	if got := res.Config.Ingress[0].Service; got != "http://direct.default.svc.cluster.local:8080" {
		t.Errorf("service = %q", got)
	}
}

func TestBuildTunnelConfigHostnameConflict(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("ns2", "later", map[string]string{"enabled": "true"}, "dup.example.com"),
		makeIngress("ns1", "earlier", map[string]string{"enabled": "true"}, "dup.example.com"),
	}, testOpts)

	if owner := res.Hostnames["dup.example.com"]; owner == nil || owner.Namespace != "ns1" {
		t.Errorf("expected ns1/earlier to win, got %+v", owner)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Reason != "HostnameConflict" {
		t.Errorf("expected one HostnameConflict warning, got %+v", res.Warnings)
	}
	// one rule + catch-all
	if len(res.Config.Ingress) != 2 {
		t.Errorf("expected 2 rules, got %d", len(res.Config.Ingress))
	}
}

func TestBuildTunnelConfigInvalidAnnotations(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "bad-scheme", map[string]string{"enabled": "true", "origin-scheme": "tcp"}, "a.example.com"),
		makeIngress("default", "bad-service", map[string]string{"enabled": "true", "origin-service": "no-scheme"}, "b.example.com"),
		makeIngress("default", "bad-bool", map[string]string{"enabled": "true", "no-tls-verify": "yes"}, "c.example.com"),
	}, testOpts)

	if len(res.Warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %+v", res.Warnings)
	}
	// bad-scheme and bad-service are skipped entirely; bad-bool falls back
	// to the default and is still published.
	if len(res.Config.Ingress) != 2 {
		t.Errorf("expected 1 rule + catch-all, got %+v", res.Config.Ingress)
	}
	if _, ok := res.Hostnames["c.example.com"]; !ok {
		t.Errorf("c.example.com should still be published")
	}
}

func TestBuildTunnelConfigWildcardHostname(t *testing.T) {
	res := BuildTunnelConfig([]netv1.Ingress{
		makeIngress("default", "wild", map[string]string{"enabled": "true"}, "*.example.com"),
	}, testOpts)

	or := res.Config.Ingress[0].OriginRequest
	// A literal "*.example.com" is not a valid SNI value, so the default
	// originServerName must not be applied to wildcard hosts.
	if or != nil && or.OriginServerName != "" {
		t.Errorf("wildcard host must not default originServerName, got %+v", or)
	}
}

func TestIngressHostsDeduplicates(t *testing.T) {
	ing := makeIngress("default", "app", nil, "a.example.com", "a.example.com", "", "b.example.com")
	hosts := ingressHosts(&ing)
	if len(hosts) != 2 || hosts[0] != "a.example.com" || hosts[1] != "b.example.com" {
		t.Errorf("hosts = %v", hosts)
	}
}
