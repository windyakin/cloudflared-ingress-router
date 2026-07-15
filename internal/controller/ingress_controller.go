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

	"github.com/windyakin/cloudflared-ingress-router/internal/cloudflare"
)

// FinalizerName guards DNS record cleanup when a published Ingress is
// deleted or opts out. Unlike the annotation prefix it is not configurable,
// so that changing the prefix does not orphan finalizers.
const FinalizerName = "cloudflared-ingress-router.windyakin.net/cleanup"

// aggregateRequest is the synthetic request every Ingress event maps to:
// the reconciler always rebuilds the whole tunnel configuration from all
// Ingresses, because the configurations API only supports full replacement.
var aggregateRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "tunnel-configuration"}}

// IngressReconciler aggregates opted-in Ingresses into the Cloudflare
// Tunnel configuration and the corresponding DNS records.
type IngressReconciler struct {
	client.Client
	Recorder       record.EventRecorder
	Cloudflare     cloudflare.API
	Opts           BuildOptions
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *IngressReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var list netv1.IngressList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ingresses: %w", err)
	}

	// Partition into Ingresses that should be published and Ingresses that
	// still carry our finalizer but should no longer be published (deleted
	// or opted out) and need DNS cleanup.
	var active, retiring []*netv1.Ingress
	for i := range list.Items {
		ing := &list.Items[i]
		enabled := IsEnabled(ing, r.Opts.AnnotationPrefix)
		deleting := !ing.DeletionTimestamp.IsZero()
		switch {
		case enabled && !deleting:
			active = append(active, ing)
		case controllerutil.ContainsFinalizer(ing, FinalizerName):
			retiring = append(retiring, ing)
		}
	}

	activeCopy := make([]netv1.Ingress, len(active))
	for i, ing := range active {
		activeCopy[i] = *ing
	}
	res := BuildTunnelConfig(activeCopy, r.Opts)
	for _, w := range res.Warnings {
		r.Recorder.Event(w.Ingress, corev1.EventTypeWarning, w.Reason, w.Message)
	}

	var errs []error

	// Make sure every published Ingress carries the finalizer before its
	// hostname goes live, so deletion can never skip DNS cleanup.
	for _, ing := range active {
		if controllerutil.ContainsFinalizer(ing, FinalizerName) {
			continue
		}
		controllerutil.AddFinalizer(ing, FinalizerName)
		if err := r.Update(ctx, ing); err != nil {
			errs = append(errs, fmt.Errorf("add finalizer to %s: %w", client.ObjectKeyFromObject(ing), err))
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
			r.Recorder.Event(owner, corev1.EventTypeWarning, "DNSRecordFailed", err.Error())
			errs = append(errs, fmt.Errorf("ensure DNS record for %s: %w", host, err))
		}
	}

	for _, ing := range retiring {
		if err := r.cleanup(ctx, ing, res.Hostnames); err != nil {
			r.Recorder.Event(ing, corev1.EventTypeWarning, "CleanupFailed", err.Error())
			errs = append(errs, fmt.Errorf("cleanup %s: %w", client.ObjectKeyFromObject(ing), err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// cleanup removes the DNS records of a retiring Ingress (unless another
// Ingress still publishes the hostname) and then releases the finalizer.
func (r *IngressReconciler) cleanup(ctx context.Context, ing *netv1.Ingress, stillDesired map[string]*netv1.Ingress) error {
	for _, host := range ingressHosts(ing) {
		if _, ok := stillDesired[host]; ok {
			continue
		}
		if err := r.Cloudflare.DeleteDNSRecord(ctx, host); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(ing, FinalizerName)
	return r.Update(ctx, ing)
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("cloudflared-ingress-router").
		Watches(&netv1.Ingress{}, handler.EnqueueRequestsFromMapFunc(
			func(context.Context, client.Object) []reconcile.Request {
				return []reconcile.Request{aggregateRequest}
			},
		)).
		Complete(r)
}
