/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strconv"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/controller"
	"github.com/einyx/kubo/internal/mcpserver"
	"github.com/einyx/kubo/internal/portal"
	"github.com/einyx/kubo/internal/prereqs"
	kubowh "github.com/einyx/kubo/internal/webhook"
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var mcpAddr string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&mcpAddr, "mcp-bind-address", ":9090", "The address the MCP server binds to.")
	var mcpToken string
	flag.StringVar(&mcpToken, "mcp-token", os.Getenv("KUBO_MCP_TOKEN"),
		"Bearer token required on MCP SSE endpoints. Empty disables auth — "+
			"bind the MCP server to localhost or a ClusterIP only.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Pre-flight, before the manager starts: with a direct (non-cached)
	// client, install cluster prereqs (Istio + Flux CRDs) and make sure the
	// webhook serving certificate exists (self-signed fallback). This must
	// happen before the HelmRelease watch and certwatcher initialize.
	preflightCfg := ctrl.GetConfigOrDie()
	preflightClient, err := client.New(preflightCfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create pre-flight client")
		os.Exit(1)
	}
	ctx := ctrl.SetupSignalHandler()
	if err := prereqs.Ensure(ctx, preflightClient); err != nil {
		setupLog.Error(err, "failed to install cluster prereqs")
		os.Exit(1)
	}
	setupLog.Info("cluster prereqs installed")
	if len(webhookCertPath) > 0 {
		webhookNS := os.Getenv("POD_NAMESPACE")
		if webhookNS == "" {
			webhookNS = "kubo-system"
		}
		if err := prereqs.EnsureWebhookCert(ctx, preflightClient,
			webhookNS, "webhook-server-cert", "kubo-webhook-service"); err != nil {
			setupLog.Error(err, "failed to ensure webhook serving certificate")
			os.Exit(1)
		}
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		certFile := filepath.Join(webhookCertPath, webhookCertName)
		keyFile := filepath.Join(webhookCertPath, webhookCertKey)
		// The Secret volume may take a moment to materialize after the
		// pre-flight created it — retry instead of exiting.
		for i := 0; ; i++ {
			webhookCertWatcher, err = certwatcher.New(certFile, keyFile)
			if err == nil {
				break
			}
			if i >= 24 { // ~2 minutes
				setupLog.Error(err, "webhook certificate never appeared", "path", certFile)
				os.Exit(1)
			}
			time.Sleep(5 * time.Second)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "d892ba06.kubo.io",
		// The manager is the last thing this binary runs, so releasing the
		// lease on shutdown is safe and lets a standby take over immediately
		// instead of waiting out LeaseDuration.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.StackDefinitionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StackDefinition")
		os.Exit(1)
	}
	helmEngine, err := controller.NewHelmEngine()
	if err != nil {
		setupLog.Error(err, "unable to create helm engine")
		os.Exit(1)
	}
	utilruntime.Must(helmv2.AddToScheme(mgr.GetScheme()))
	utilruntime.Must(sourcev1.AddToScheme(mgr.GetScheme()))
	if err = (&controller.StackReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Helm:   helmEngine,
		Flux:   &controller.FluxStrategy{Client: mgr.GetClient()},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Stack")
		os.Exit(1)
	}
	webhookServer.Register("/provision", &kubowh.ProvisioningHandler{
		Client:    mgr.GetClient(),
		SecretKey: os.Getenv("WEBHOOK_SECRET"),
	})
	if err = platformv1alpha1.SetupStackWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Stack")
		os.Exit(1)
	}
	if err = (&controller.StackBackupReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StackBackup")
		os.Exit(1)
	}
	// Demo tenants from website requests: reconcile DemoRequests into
	// template-rendered Stacks. Email credentials come from a Secret; when
	// absent, notification is disabled and tenants still provision (the URL
	// is visible on the DemoRequest status).
	demoEmailer := controller.DemoEmailerFromSecret(ctx, mgr.GetClient())
	demoReqs := &controller.DemoRequestReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Registry:        portal.NewRegistry("", mgr.GetClient()),
		Emailer:         demoEmailer,
		MaxTenants:      atoiEnv("DEMO_MAX_TENANTS", 10),
		DefaultTemplate: envOr("DEMO_DEFAULT_TEMPLATE", "full"),
		TenantDomain:    envOr("DEMO_TENANT_DOMAIN", "meshx.foundation"),
	}
	if demoEmailer == nil {
		setupLog.Info("demo request emailer disabled (no kubo-system/demo-request-email secret)")
	}
	// Subscribe/pull transport: drain website demo requests from a Storage
	// Queue when configured (env or kubo-system/demo-request-queue secret).
	// The push transport (POST /api/demorequests on the portal) is always on.
	if err := controller.StartQueueIngesterFromEnv(ctx, mgr.GetClient()); err != nil {
		setupLog.Error(err, "unable to start demo request queue ingester")
		os.Exit(1)
	}
	if err = demoReqs.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DemoRequest")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Add(func() manager.Runnable {
		srv := mcpserver.New(mgr.GetClient(), mcpAddr)
		srv.Token = mcpToken
		srv.Portal = portal.New(mgr.GetClient())
		return srv
	}()); err != nil {
		setupLog.Error(err, "unable to add MCP server")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// atoiEnv parses an integer env var, falling back to def.
func atoiEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envOr returns the env var or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
