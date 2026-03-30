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
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
	dorguwebhook "github.com/dorgu-ai/dorgu-operator/internal/webhook"
	dorguws "github.com/dorgu-ai/dorgu-operator/internal/websocket"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(dorguv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	cfg := parseFlags()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cfg.zapOpts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	var tlsOpts []func(*tls.Config)
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !cfg.enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(cfg.webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.webhookCertPath, "webhook-cert-name", cfg.webhookCertName, "webhook-cert-key", cfg.webhookCertKey)

		webhookServerOptions.CertDir = cfg.webhookCertPath
		webhookServerOptions.CertName = cfg.webhookCertName
		webhookServerOptions.KeyName = cfg.webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if cfg.secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
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
	if len(cfg.metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.metricsCertPath, "metrics-cert-name", cfg.metricsCertName, "metrics-cert-key", cfg.metricsCertKey)

		metricsServerOptions.CertDir = cfg.metricsCertPath
		metricsServerOptions.CertName = cfg.metricsCertName
		metricsServerOptions.KeyName = cfg.metricsCertKey
	}

	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElection,
		LeaderElectionID:       "48ec518d.dorgu.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create discovery client for ClusterPersona controller
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to create discovery client")
		os.Exit(1)
	}

	if err := (&controller.ApplicationPersonaReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PrometheusURL: cfg.prometheusURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ApplicationPersona")
		os.Exit(1)
	}

	if cfg.prometheusURL != "" {
		setupLog.Info("Prometheus integration enabled", "url", cfg.prometheusURL)
	}

	if err := (&controller.ClusterPersonaReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		DiscoveryClient: discoveryClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterPersona")
		os.Exit(1)
	}

	// Register ArgoCD watcher if enabled and ArgoCD CRD is present
	if cfg.enableArgoCD {
		if controller.ArgoCDCRDExists(discoveryClient) {
			setupLog.Info("ArgoCD integration enabled, setting up watcher")
			if err := (&controller.ArgoCDWatcher{
				Client:  mgr.GetClient(),
				Scheme:  mgr.GetScheme(),
				Enabled: true,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Info("ArgoCD watcher setup failed", "error", err.Error())
			}
		} else {
			setupLog.Info("ArgoCD integration disabled: Application CRD (argoproj.io/v1alpha1) not found in cluster")
		}
	}

	// Register optional Deployment validating webhook
	if cfg.enableWebhook {
		mode := dorguwebhook.ModeAdvisory
		if cfg.webhookMode == "enforcing" {
			mode = dorguwebhook.ModeEnforcing
		}
		setupLog.Info("Registering Deployment validating webhook", "mode", mode)
		mgr.GetWebhookServer().Register("/validate-deployment", &webhook.Admission{
			Handler: &dorguwebhook.DeploymentValidator{
				Client: mgr.GetClient(),
				Mode:   mode,
			},
		})
	}
	// Phase 2a: Health check reconciler + event pipeline + incident controller.
	// All components gated behind --enable-health-check flag.
	if cfg.enableHealthCheck {
		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			setupLog.Error(err, "unable to create kubernetes clientset")
			os.Exit(1)
		}

		// 1. Create detection collectors.
		nodeCollector := detection.NewNodeCollector(mgr.GetClient(), setupLog)
		podCollector := detection.NewPodCollector(mgr.GetClient(), setupLog)
		resourceCollector := detection.NewResourceCollector(mgr.GetClient(), setupLog)
		controlPlaneCollector := detection.NewControlPlaneCollector(
			mgr.GetClient(), clientset.CoreV1().RESTClient(), setupLog,
		)

		collectors := []detection.SignalCollector{
			nodeCollector, podCollector, resourceCollector, controlPlaneCollector,
		}

		// Optional: metrics-server collector.
		if cfg.enableMetricsServer {
			mc, err := metricsclient.NewForConfig(restConfig)
			if err != nil {
				setupLog.Info("metrics-server client creation failed, skipping metrics collector",
					"error", err.Error())
			} else {
				metricsCollector := detection.NewMetricsCollector(mc, mgr.GetClient(), setupLog)
				collectors = append(collectors, metricsCollector)
			}
		}

		// 2. Create detection engine.
		detectionEngine := detection.NewEngine(setupLog, collectors...)

		// 3. Create diagnosis engine.
		ruleProvider := diagnosis.NewRuleBasedProvider(setupLog)
		diagnosisEngine := diagnosis.NewEngine(setupLog, ruleProvider)

		// 4. Create event pipeline components.
		classifier := events.NewClassifier()
		correlator := events.NewCorrelator(mgr.GetClient())
		eventStore := events.NewEventStore(mgr.GetClient(), setupLog)
		emitter := events.NewEmitter(mgr.GetEventRecorderFor("dorgu-operator"), setupLog)

		// 5. Start event watcher.
		eventWatcher := events.NewWatcher(clientset, classifier, correlator, eventStore, emitter, setupLog)
		if err := mgr.Add(eventWatcher); err != nil {
			setupLog.Error(err, "unable to start event watcher")
			os.Exit(1)
		}

		// 6. Start TTL cleanup.
		cleaner := events.NewCleaner(mgr.GetClient(), setupLog)
		if err := mgr.Add(cleaner); err != nil {
			setupLog.Error(err, "unable to start event cleaner")
			os.Exit(1)
		}

		// 7. Start health check reconciler.
		healthReconciler := &controller.HealthCheckReconciler{
			Client:            mgr.GetClient(),
			Detection:         detectionEngine,
			Diagnosis:         diagnosisEngine,
			EventStore:        eventStore,
			EventEmitter:      emitter,
			Logger:            setupLog.WithName("health-check"),
			ReconcileInterval: cfg.healthCheckInterval,
		}
		if err := mgr.Add(healthReconciler); err != nil {
			setupLog.Error(err, "unable to start health check reconciler")
			os.Exit(1)
		}

		// 8. Register incident controller.
		if err := (&controller.IncidentController{
			Client: mgr.GetClient(),
			Logger: setupLog.WithName("incident-controller"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create incident controller")
			os.Exit(1)
		}

		setupLog.Info("Phase 2a health check reconciler enabled",
			"interval", cfg.healthCheckInterval,
			"metricsServer", cfg.enableMetricsServer,
		)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Start WebSocket server if enabled
	if cfg.enableWebSocket {
		setupLog.Info("Starting WebSocket server", "addr", cfg.webSocketAddr)
		wsServer := dorguws.NewServer(mgr.GetClient(), cfg.webSocketAddr)
		go func() {
			if err := wsServer.Start(ctrl.SetupSignalHandler()); err != nil {
				setupLog.Error(err, "WebSocket server error")
			}
		}()
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
