package controller

import (
	"fmt"
	"sort"
	"strings"

	netv1 "k8s.io/api/networking/v1"

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

// BuildWarning is a per-Ingress problem found while building the
// configuration. The reconciler surfaces these as Kubernetes events.
type BuildWarning struct {
	Ingress *netv1.Ingress
	Reason  string
	Message string
}

// BuildResult is the outcome of BuildTunnelConfig.
type BuildResult struct {
	Config *cloudflare.TunnelConfig
	// Hostnames maps each published hostname to the Ingress that owns it.
	Hostnames map[string]*netv1.Ingress
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
// tunnel configuration. Ingresses are processed in namespace/name order and
// the first owner of a hostname wins; later claims produce a warning.
// The returned rule list is sorted by hostname and always ends with the
// mandatory catch-all rule.
func BuildTunnelConfig(ingresses []netv1.Ingress, opts BuildOptions) BuildResult {
	res := BuildResult{
		Config:    &cloudflare.TunnelConfig{},
		Hostnames: map[string]*netv1.Ingress{},
	}

	sorted := make([]*netv1.Ingress, 0, len(ingresses))
	for i := range ingresses {
		if IsEnabled(&ingresses[i], opts.AnnotationPrefix) {
			sorted = append(sorted, &ingresses[i])
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Name < sorted[j].Name
	})

	for _, ing := range sorted {
		service, warn := originService(ing, opts)
		if warn != nil {
			res.Warnings = append(res.Warnings, *warn)
			continue
		}
		originRequest, warns := buildOriginRequest(ing, opts, service)
		res.Warnings = append(res.Warnings, warns...)

		for _, host := range ingressHosts(ing) {
			if owner, taken := res.Hostnames[host]; taken {
				res.Warnings = append(res.Warnings, BuildWarning{
					Ingress: ing,
					Reason:  "HostnameConflict",
					Message: fmt.Sprintf("hostname %s is already published by Ingress %s/%s; skipping", host, owner.Namespace, owner.Name),
				})
				continue
			}
			res.Hostnames[host] = ing
			rule := cloudflare.IngressRule{
				Hostname: host,
				Service:  service,
			}
			if originRequest != nil {
				or := *originRequest
				// Default the expected origin certificate name to the
				// published hostname so that Traefik (serving per-host
				// certificates via SNI) passes TLS verification.
				if strings.HasPrefix(service, "https://") && or.OriginServerName == "" && !strings.Contains(host, "*") {
					or.OriginServerName = host
				}
				rule.OriginRequest = &or
			}
			res.Config.Ingress = append(res.Config.Ingress, rule)
		}
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
				Ingress: ing,
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
			Ingress: ing,
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
				Ingress: ing,
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
