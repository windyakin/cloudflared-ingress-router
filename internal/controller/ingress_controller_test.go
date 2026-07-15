package controller

import (
	"context"
	"testing"
	"time"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/windyakin/cloudflared-ingress-router/internal/cloudflare"
)

func newTestReconciler(t *testing.T, objs ...*netv1.Ingress) (*IngressReconciler, *cloudflare.Fake) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	fakeAPI := cloudflare.NewFake()
	r := &IngressReconciler{
		Client:         builder.Build(),
		Recorder:       record.NewFakeRecorder(32),
		Cloudflare:     fakeAPI,
		Opts:           testOpts,
		ResyncInterval: 10 * time.Minute,
	}
	return r, fakeAPI
}

func runReconcile(t *testing.T, r *IngressReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	return res
}

func TestReconcilePublishesIngress(t *testing.T) {
	ing := makeIngress("default", "app", map[string]string{"enabled": "true"}, "app.example.com")
	r, api := newTestReconciler(t, &ing)

	res := runReconcile(t, r)

	if res.RequeueAfter != 10*time.Minute {
		t.Errorf("RequeueAfter = %v, want resync interval", res.RequeueAfter)
	}
	if api.UpdateCalls != 1 {
		t.Errorf("UpdateCalls = %d, want 1", api.UpdateCalls)
	}
	if len(api.Config.Ingress) != 2 || api.Config.Ingress[0].Hostname != "app.example.com" {
		t.Errorf("tunnel config = %+v", api.Config.Ingress)
	}
	if !api.Records["app.example.com"] {
		t.Errorf("DNS record not created: %v", api.Records)
	}

	var got netv1.Ingress
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app"}, &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, FinalizerName) {
		t.Errorf("finalizer not added: %v", got.Finalizers)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	ing := makeIngress("default", "app", map[string]string{"enabled": "true"}, "app.example.com")
	r, api := newTestReconciler(t, &ing)

	runReconcile(t, r)
	runReconcile(t, r)

	if api.UpdateCalls != 1 {
		t.Errorf("UpdateCalls = %d, want 1 (second reconcile must be a no-op)", api.UpdateCalls)
	}
}

func TestReconcileCleansUpDeletedIngress(t *testing.T) {
	ing := makeIngress("default", "app", map[string]string{"enabled": "true"}, "app.example.com")
	now := metav1.Now()
	ing.DeletionTimestamp = &now
	ing.Finalizers = []string{FinalizerName}
	r, api := newTestReconciler(t, &ing)
	api.Records["app.example.com"] = true
	api.Config = cloudflare.TunnelConfig{Ingress: []cloudflare.IngressRule{
		{Hostname: "app.example.com", Service: testOpts.OriginURLHTTPS},
		{Service: "http_status:404"},
	}}

	runReconcile(t, r)

	if api.Records["app.example.com"] {
		t.Errorf("DNS record not deleted: %v", api.Records)
	}
	if len(api.Config.Ingress) != 1 || api.Config.Ingress[0].Service != "http_status:404" {
		t.Errorf("tunnel config not reduced to catch-all: %+v", api.Config.Ingress)
	}
	// Removing the last finalizer lets the fake client actually delete it.
	var got netv1.Ingress
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app"}, &got)
	if err == nil {
		t.Errorf("ingress still exists with finalizers %v", got.Finalizers)
	}
}

func TestReconcileCleansUpOptedOutIngress(t *testing.T) {
	ing := makeIngress("default", "app", map[string]string{"enabled": "false"}, "app.example.com")
	ing.Finalizers = []string{FinalizerName}
	r, api := newTestReconciler(t, &ing)
	api.Records["app.example.com"] = true

	runReconcile(t, r)

	if api.Records["app.example.com"] {
		t.Errorf("DNS record not deleted: %v", api.Records)
	}
	var got netv1.Ingress
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app"}, &got); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&got, FinalizerName) {
		t.Errorf("finalizer not removed: %v", got.Finalizers)
	}
}

func TestReconcileKeepsSharedHostnameOnCleanup(t *testing.T) {
	// Two Ingresses publish the same hostname; deleting the loser must not
	// remove the DNS record still used by the winner.
	winner := makeIngress("ns1", "winner", map[string]string{"enabled": "true"}, "dup.example.com")
	loser := makeIngress("ns2", "loser", map[string]string{"enabled": "true"}, "dup.example.com")
	now := metav1.Now()
	loser.DeletionTimestamp = &now
	loser.Finalizers = []string{FinalizerName}
	r, api := newTestReconciler(t, &winner, &loser)

	runReconcile(t, r)

	if !api.Records["dup.example.com"] {
		t.Errorf("shared DNS record must survive cleanup: %v", api.Records)
	}
}
