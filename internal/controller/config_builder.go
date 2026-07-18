package controller

import (
	"fmt"
	"sort"
	"strings"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/windyakin/cloudflared-ingress-router/internal/api/v1alpha1"
	"github.com/windyakin/cloudflared-ingress-router/internal/cloudflare"
)

// Annotation suffixes, combined with the configurable prefix as
// "<prefix>/<suffix>".
const (
	annotationEnabled          = "enabled"
	annotationOriginScheme     = "origin-scheme"
	annotationOriginService    = "origin-service"
	annotationNoTLSVerify      = "no-tls-verify"
	annotationOriginServerName = "origin-server-name"
	annotationHTTPHostHeader   = "http-host-header"
	annotationHTTP2Origin      = "http2-origin"
	annotationCAPool           = "ca-pool"
)

const catchAllService = "http_status:404"

// BuildOptions carries the controller-level defaults for building the
// tunnel configuration.
type BuildOptions struct {
	// AnnotationPrefix is the domain part of the annotations, without the
	// trailing slash (e.g. "cloudflared-ingress-router.windyakin.net").
	AnnotationPrefix string
	// OriginURLHTTPS is the default origin (usually the Traefik service)
	// used when origin-scheme is https.
	OriginURLHTTPS string
	// OriginURLHTTP is the default origin used when origin-scheme is http.
	OriginURLHTTP string
}

// RouteSource identifies the Kubernetes object that produced a routing rule.
type RouteSource struct {
	Kind      string
	Namespace string
	Name      string
	Object    runtime.Object
}

func (s RouteSource) sortKey() string {
	return s.Kind + "/" + s.Namespace + "/" + s.Name
}

// candidateRule pairs a routing rule with the source object that produced it.
type candidateRule struct {
	source RouteSource
	rule   cloudflare.IngressRule
}

// BuildWarning is a per-resource problem found while building the
// configuration. The reconciler surfaces these as Kubernetes events.
type BuildWarning struct {
	Object  runtime.Object
	Reason  string
	Message string
}

// BuildResult is the outcome of BuildTunnelConfig / BuildTunnelConfigFromCandidates.
type BuildResult struct {
	Config    *cloudflare.TunnelConfig
	Hostnames map[string]RouteSource
	Warnings  []BuildWarning
}

// IsEnabled reports whether an Ingress opts in to publication.
func IsEnabled(ing *netv1.Ingress, prefix string) bool {
	return ing.Annotations[prefix+"/"+annotationEnabled] == "true"
}

func annotation(ing *netv1.Ingress, prefix, suffix string) (string, bool) {
	v, ok := ing.Annotations[prefix+"/"+suffix]
	return v, ok
}

// ingressHosts returns the deduplicated, non-empty hostnames of an Ingress.
func ingressHosts(ing *netv1.Ingress) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" || seen[rule.Host] {
			continue
		}
		seen[rule.Host] = true
		hosts = append(hosts, rule.Host)
	}
	return hosts
}

// BuildTunnelConfig aggregates all opted-in Ingresses into a single desired
// tunnel configuration. It delegates to BuildCandidatesFromIngresses and
// BuildTunnelConfigFromCandidates.
func BuildTunnelConfig(ingresses []netv1.Ingress, opts BuildOptions) BuildResult {
	candidates, warnings := BuildCandidatesFromIngresses(ingresses, opts)
	res := BuildTunnelConfigFromCandidates(candidates)
	res.Warnings = append(warnings, res.Warnings...)
	return res
}

// BuildCandidatesFromIngresses converts opted-in Ingresses into candidate
// rules. Ingresses without the enabled annotation are skipped. Annotation
// validation warnings are returned alongside the candidates.
func BuildCandidatesFromIngresses(ingresses []netv1.Ingress, opts BuildOptions) ([]candidateRule, []BuildWarning) {
	var candidates []candidateRule
	var warnings []BuildWarning

	for i := range ingresses {
		ing := &ingresses[i]
		if !IsEnabled(ing, opts.AnnotationPrefix) {
			continue
		}

		service, warn := originService(ing, opts)
		if warn != nil {
			warnings = append(warnings, *warn)
			continue
		}
		originReq, warns := buildOriginRequest(ing, opts, service)
		warnings = append(warnings, warns...)

		source := RouteSource{
			Kind:      "Ingress",
			Namespace: ing.Namespace,
			Name:      ing.Name,
			Object:    ing,
		}

		for _, host := range ingressHosts(ing) {
			rule := cloudflare.IngressRule{
				Hostname: host,
				Service:  service,
			}
			if originReq != nil {
				or := *originReq
				if strings.HasPrefix(service, "https://") && or.OriginServerName == "" && !strings.Contains(host, "*") {
					or.OriginServerName = host
				}
				rule.OriginRequest = &or
			}
			candidates = append(candidates, candidateRule{source: source, rule: rule})
		}
	}

	return candidates, warnings
}

// BuildCandidatesFromTunnelRoutes converts TunnelRoute resources into
// candidate rules. Every TunnelRoute is active by its existence (no
// opt-in annotation).
func BuildCandidatesFromTunnelRoutes(routes []v1alpha1.TunnelRoute) []candidateRule {
	var candidates []candidateRule
	for i := range routes {
		tr := &routes[i]
		source := RouteSource{
			Kind:      "TunnelRoute",
			Namespace: tr.Namespace,
			Name:      tr.Name,
			Object:    tr,
		}
		for _, r := range tr.Spec.Rules {
			rule := cloudflare.IngressRule{
				Hostname: r.Hostname,
				Service:  r.Service,
			}
			if r.OriginRequest != nil {
				rule.OriginRequest = &cloudflare.OriginRequest{
					NoTLSVerify:      r.OriginRequest.NoTLSVerify,
					OriginServerName: r.OriginRequest.OriginServerName,
					HTTPHostHeader:   r.OriginRequest.HTTPHostHeader,
					HTTP2Origin:      r.OriginRequest.HTTP2Origin,
					CAPool:           r.OriginRequest.CAPool,
				}
			}
			candidates = append(candidates, candidateRule{source: source, rule: rule})
		}
	}
	return candidates
}

// BuildTunnelConfigFromCandidates merges candidate rules from all sources
// into a single tunnel configuration. Candidates are sorted by
// Kind/Namespace/Name so the first owner of a hostname wins deterministically.
// The returned rule list is sorted by hostname and always ends with the
// mandatory catch-all rule.
func BuildTunnelConfigFromCandidates(candidates []candidateRule) BuildResult {
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].source.sortKey() < candidates[j].source.sortKey()
	})

	res := BuildResult{
		Config:    &cloudflare.TunnelConfig{},
		Hostnames: map[string]RouteSource{},
	}

	for _, c := range candidates {
		host := c.rule.Hostname
		if owner, taken := res.Hostnames[host]; taken {
			res.Warnings = append(res.Warnings, BuildWarning{
				Object:  c.source.Object,
				Reason:  "HostnameConflict",
				Message: fmt.Sprintf("hostname %s is already published by %s %s/%s; skipping", host, owner.Kind, owner.Namespace, owner.Name),
			})
			continue
		}
		res.Hostnames[host] = c.source
		res.Config.Ingress = append(res.Config.Ingress, c.rule)
	}

	sort.Slice(res.Config.Ingress, func(i, j int) bool {
		return res.Config.Ingress[i].Hostname < res.Config.Ingress[j].Hostname
	})
	res.Config.Ingress = append(res.Config.Ingress, cloudflare.IngressRule{Service: catchAllService})
	return res
}

// originService resolves the origin service URL of an Ingress from its
// annotations and the controller defaults.
func originService(ing *netv1.Ingress, opts BuildOptions) (string, *BuildWarning) {
	if svc, ok := annotation(ing, opts.AnnotationPrefix, annotationOriginService); ok {
		if !strings.Contains(svc, "://") {
			return "", &BuildWarning{
				Object: ing,
				Reason:  "InvalidAnnotation",
				Message: fmt.Sprintf("%s/%s must be a URL including a scheme (got %q); skipping Ingress", opts.AnnotationPrefix, annotationOriginService, svc),
			}
		}
		return svc, nil
	}
	scheme, ok := annotation(ing, opts.AnnotationPrefix, annotationOriginScheme)
	if !ok {
		scheme = "https"
	}
	switch scheme {
	case "https":
		return opts.OriginURLHTTPS, nil
	case "http":
		return opts.OriginURLHTTP, nil
	default:
		return "", &BuildWarning{
			Object: ing,
			Reason:  "InvalidAnnotation",
			Message: fmt.Sprintf("%s/%s must be \"http\" or \"https\" (got %q); skipping Ingress", opts.AnnotationPrefix, annotationOriginScheme, scheme),
		}
	}
}

// buildOriginRequest translates the TLS/HTTP annotations of an Ingress into
// an OriginRequest template shared by all of its hostnames. It returns nil
// when no relevant annotation is set and the service is not HTTPS.
func buildOriginRequest(ing *netv1.Ingress, opts BuildOptions, service string) (*cloudflare.OriginRequest, []BuildWarning) {
	var warns []BuildWarning
	or := cloudflare.OriginRequest{}

	parseBool := func(suffix string) bool {
		v, ok := annotation(ing, opts.AnnotationPrefix, suffix)
		if !ok {
			return false
		}
		switch v {
		case "true":
			return true
		case "false":
			return false
		default:
			warns = append(warns, BuildWarning{
				Object: ing,
				Reason:  "InvalidAnnotation",
				Message: fmt.Sprintf("%s/%s must be \"true\" or \"false\" (got %q); treating as false", opts.AnnotationPrefix, suffix, v),
			})
			return false
		}
	}

	or.NoTLSVerify = parseBool(annotationNoTLSVerify)
	or.HTTP2Origin = parseBool(annotationHTTP2Origin)
	if v, ok := annotation(ing, opts.AnnotationPrefix, annotationOriginServerName); ok {
		or.OriginServerName = v
	}
	if v, ok := annotation(ing, opts.AnnotationPrefix, annotationHTTPHostHeader); ok {
		or.HTTPHostHeader = v
	}
	if v, ok := annotation(ing, opts.AnnotationPrefix, annotationCAPool); ok {
		or.CAPool = v
	}

	if or == (cloudflare.OriginRequest{}) && !strings.HasPrefix(service, "https://") {
		return nil, warns
	}
	return &or, warns
}
