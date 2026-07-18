package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/windyakin/cloudflared-ingress-router/internal/api/v1alpha1"
	"github.com/windyakin/cloudflared-ingress-router/internal/cloudflare"
)

// FinalizerName guards DNS record cleanup when a published Ingress or
// TunnelRoute is deleted or opts out. Unlike the annotation prefix it is
// not configurable, so that changing the prefix does not orphan finalizers.
const FinalizerName = "cloudflared-ingress-router.windyakin.net/cleanup"

// aggregateRequest is the synthetic request every Ingress / TunnelRoute
// event maps to: the reconciler always rebuilds the whole tunnel
// configuration from all sources, because the configurations API only
// supports full replacement.
var aggregateRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "tunnel-configuration"}}

// IngressReconciler aggregates opted-in Ingresses and TunnelRoutes into
// the Cloudflare Tunnel configuration and the corresponding DNS records.
type IngressReconciler struct {
	client.Client
	Recorder       record.EventRecorder
	Cloudflare     cloudflare.API
	Opts           BuildOptions
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloudflared-ingress-router.windyakin.net,resources=tunnelroutes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *IngressReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// --- Ingresses ---
	var ingList netv1.IngressList
	if err := r.List(ctx, &ingList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ingresses: %w", err)
	}

	var activeIngresses, retiringIngresses []*netv1.Ingress
	for i := range ingList.Items {
		ing := &ingList.Items[i]
		enabled := IsEnabled(ing, r.Opts.AnnotationPrefix)
		deleting := !ing.DeletionTimestamp.IsZero()
		switch {
		case enabled && !deleting:
			activeIngresses = append(activeIngresses, ing)
		case controllerutil.ContainsFinalizer(ing, FinalizerName):
			retiringIngresses = append(retiringIngresses, ing)
		}
	}

	// --- TunnelRoutes ---
	var trList v1alpha1.TunnelRouteList
	if err := r.List(ctx, &trList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list tunnelroutes: %w", err)
	}

	var activeTunnelRoutes, retiringTunnelRoutes []*v1alpha1.TunnelRoute
	for i := range trList.Items {
		tr := &trList.Items[i]
		deleting := !tr.DeletionTimestamp.IsZero()
		switch {
		case !deleting:
			activeTunnelRoutes = append(activeTunnelRoutes, tr)
		case controllerutil.ContainsFinalizer(tr, FinalizerName):
			retiringTunnelRoutes = append(retiringTunnelRoutes, tr)
		}
	}

	// --- Build unified config ---
	ingCopy := make([]netv1.Ingress, len(activeIngresses))
	for i, ing := range activeIngresses {
		ingCopy[i] = *ing
	}
	trCopy := make([]v1alpha1.TunnelRoute, len(activeTunnelRoutes))
	for i, tr := range activeTunnelRoutes {
		trCopy[i] = *tr
	}

	ingCandidates, warnings := BuildCandidatesFromIngresses(ingCopy, r.Opts)
	trCandidates := BuildCandidatesFromTunnelRoutes(trCopy)
	allCandidates := append(ingCandidates, trCandidates...)
	res := BuildTunnelConfigFromCandidates(allCandidates)
	res.Warnings = append(warnings, res.Warnings...)

	for _, w := range res.Warnings {
		r.Recorder.Event(w.Object, corev1.EventTypeWarning, w.Reason, w.Message)
	}

	var errs []error

	// Add finalizers to active Ingresses.
	for _, ing := range activeIngresses {
		if controllerutil.ContainsFinalizer(ing, FinalizerName) {
			continue
		}
		controllerutil.AddFinalizer(ing, FinalizerName)
		if err := r.Update(ctx, ing); err != nil {
			errs = append(errs, fmt.Errorf("add finalizer to %s: %w", client.ObjectKeyFromObject(ing), err))
		}
	}
	// Add finalizers to active TunnelRoutes.
	for _, tr := range activeTunnelRoutes {
		if controllerutil.ContainsFinalizer(tr, FinalizerName) {
			continue
		}
		controllerutil.AddFinalizer(tr, FinalizerName)
		if err := r.Update(ctx, tr); err != nil {
			errs = append(errs, fmt.Errorf("add finalizer to %s: %w", client.ObjectKeyFromObject(tr), err))
		}
	}

	current, err := r.Cloudflare.GetTunnelConfig(ctx)
	if err != nil {
		return ctrl.Result{}, errors.Join(append(errs, err)...)
	}
	if !res.Config.Equal(current) {
		if err := r.Cloudflare.UpdateTunnelConfig(ctx, res.Config); err != nil {
			return ctrl.Result{}, errors.Join(append(errs, err)...)
		}
		log.Info("tunnel configuration updated", "rules", len(res.Config.Ingress))
	}

	for host, owner := range res.Hostnames {
		if err := r.Cloudflare.EnsureDNSRecord(ctx, host); err != nil {
			r.Recorder.Event(owner.Object, corev1.EventTypeWarning, "DNSRecordFailed", err.Error())
			errs = append(errs, fmt.Errorf("ensure DNS record for %s: %w", host, err))
		}
	}

	// Cleanup retiring Ingresses.
	for _, ing := range retiringIngresses {
		if err := r.cleanupHosts(ctx, ing, ingressHosts(ing), res.Hostnames); err != nil {
			r.Recorder.Event(ing, corev1.EventTypeWarning, "CleanupFailed", err.Error())
			errs = append(errs, fmt.Errorf("cleanup %s: %w", client.ObjectKeyFromObject(ing), err))
		}
	}
	// Cleanup retiring TunnelRoutes.
	for _, tr := range retiringTunnelRoutes {
		if err := r.cleanupHosts(ctx, tr, tunnelRouteHosts(tr), res.Hostnames); err != nil {
			r.Recorder.Event(tr, corev1.EventTypeWarning, "CleanupFailed", err.Error())
			errs = append(errs, fmt.Errorf("cleanup %s: %w", client.ObjectKeyFromObject(tr), err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// tunnelRouteHosts returns the hostnames of a TunnelRoute.
func tunnelRouteHosts(tr *v1alpha1.TunnelRoute) []string {
	hosts := make([]string, 0, len(tr.Spec.Rules))
	for _, r := range tr.Spec.Rules {
		if r.Hostname != "" {
			hosts = append(hosts, r.Hostname)
		}
	}
	return hosts
}

// cleanupHosts removes the DNS records for the given hostnames (unless still
// desired) and then releases the finalizer from the object.
func (r *IngressReconciler) cleanupHosts(ctx context.Context, obj client.Object, hosts []string, stillDesired map[string]RouteSource) error {
	for _, host := range hosts {
		if _, ok := stillDesired[host]; ok {
			continue
		}
		if err := r.Cloudflare.DeleteDNSRecord(ctx, host); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(obj, FinalizerName)
	return r.Update(ctx, obj)
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToAggregate := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{aggregateRequest}
		},
	)
	return ctrl.NewControllerManagedBy(mgr).
		Named("cloudflared-ingress-router").
		Watches(&netv1.Ingress{}, mapToAggregate).
		Watches(&v1alpha1.TunnelRoute{}, mapToAggregate).
		Complete(r)
}
