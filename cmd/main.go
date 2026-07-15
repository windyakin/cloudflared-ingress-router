package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/windyakin/cloudflared-ingress-router/internal/cloudflare"
	"github.com/windyakin/cloudflared-ingress-router/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var (
		accountID            string
		tunnelID             string
		annotationPrefix     string
		originURLHTTPS       string
		originURLHTTP        string
		resyncInterval       time.Duration
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
	)
	flag.StringVar(&accountID, "account-id", "", "Cloudflare account ID (required)")
	flag.StringVar(&tunnelID, "tunnel-id", "", "Cloudflare Tunnel ID (required)")
	flag.StringVar(&annotationPrefix, "annotation-prefix", "cloudflared-ingress-router.windyakin.net", "Prefix (domain part) of the Ingress annotations")
	flag.StringVar(&originURLHTTPS, "origin-url-https", "https://traefik.kube-system.svc.cluster.local:443", "Default origin URL used when origin-scheme is https")
	flag.StringVar(&originURLHTTP, "origin-url-http", "http://traefik.kube-system.svc.cluster.local:80", "Default origin URL used when origin-scheme is http")
	flag.DurationVar(&resyncInterval, "resync-interval", 10*time.Minute, "Interval of the periodic reconciliation that corrects drift")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	switch {
	case apiToken == "":
		setupLog.Error(nil, "CLOUDFLARE_API_TOKEN environment variable is required")
		os.Exit(1)
	case accountID == "":
		setupLog.Error(nil, "--account-id is required")
		os.Exit(1)
	case tunnelID == "":
		setupLog.Error(nil, "--tunnel-id is required")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cloudflared-ingress-router.windyakin.net",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconciler := &controller.IngressReconciler{
		Client:     mgr.GetClient(),
		Recorder:   mgr.GetEventRecorderFor("cloudflared-ingress-router"),
		Cloudflare: cloudflare.NewClient(apiToken, accountID, tunnelID),
		Opts: controller.BuildOptions{
			AnnotationPrefix: annotationPrefix,
			OriginURLHTTPS:   originURLHTTPS,
			OriginURLHTTP:    originURLHTTP,
		},
		ResyncInterval: resyncInterval,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ingress")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
