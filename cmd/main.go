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
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
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

	// Create WebSocket server upfront (before health check) so it can be injected
	// into all controllers that need to broadcast lifecycle events.
	var wsServer *dorguws.Server
	if cfg.enableWebSocket {
		setupLog.Info("Starting WebSocket server", "addr", cfg.webSocketAddr)
		wsServer = dorguws.NewServer(mgr.GetClient(), cfg.webSocketAddr)
	}

	if err := (&controller.ApplicationPersonaReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PrometheusURL: cfg.prometheusURL,
		WebSocket:     wsServer,
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
	// Create a single signal-handler context shared by all components.
	// ctrl.SetupSignalHandler() must only be called once — a second call
	// panics with "close of closed channel".
	signalCtx := ctrl.SetupSignalHandler()

	// Start the WebSocket server (already constructed above so it can be
	// injected into the ApplicationPersonaReconciler before Setup).
	if wsServer != nil {
		go func() {
			if err := wsServer.Start(signalCtx); err != nil {
				setupLog.Error(err, "WebSocket server error")
			}
		}()
	}

	// Auto-create ClusterPersona on startup if none exists (ArgoCD-style bootstrap).
	if cfg.autoCreateClusterPersona {
		bootstrap := &controller.ClusterPersonaBootstrap{
			Client:         mgr.GetClient(),
			Log:            setupLog.WithName("bootstrap"),
			EnsureInterval: cfg.clusterPersonaEnsureInterval,
		}
		if err := mgr.Add(bootstrap); err != nil {
			setupLog.Error(err, "unable to add ClusterPersona bootstrap runnable")
			os.Exit(1)
		}
		// The runnable is leader-gated (NeedLeaderElection=true), so it will not run
		// until this manager wins the lease. Make that wait observable in the logs.
		setupLog.Info("ClusterPersona bootstrap registered; will run after leader election",
			"ensureInterval", cfg.clusterPersonaEnsureInterval.String())
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
		detectionEngine.SetPersonaCorrelator(
			detection.NewPersonaCorrelator(mgr.GetClient(), setupLog.WithName("persona-correlator")),
		)

		// 3. Create diagnosis engine with providers.
		var diagnosisProviders []diagnosis.DiagnosisProvider
		diagnosisProviders = append(diagnosisProviders, diagnosis.NewRuleBasedProvider(setupLog))

		// aiPlanner, when set, enables AI-generated ordered remediation plans in
		// the proposer (constructed below). It degrades gracefully to rules.
		var aiPlanner planner.Planner

		if cfg.llmProvider != "" {
			apiKey := cfg.llmAPIKey
			if apiKey == "" {
				switch cfg.llmProvider {
				case "claude":
					apiKey = os.Getenv("ANTHROPIC_API_KEY")
				case "gemini":
					apiKey = os.Getenv("GEMINI_API_KEY")
				}
			}
			if apiKey != "" {
				llmClient, llmErr := llm.NewClient(cfg.llmProvider, apiKey)
				if llmErr != nil {
					setupLog.Error(llmErr, "failed to create LLM client, continuing without AI diagnosis")
				} else {
					if cfg.llmModel != "" {
						if setter, ok := llmClient.(interface{ SetModel(string) }); ok {
							setter.SetModel(cfg.llmModel)
						}
					}
					diagnosisProviders = append(diagnosisProviders, diagnosis.NewAIProvider(llmClient, setupLog))
					setupLog.Info("AI diagnosis enabled", "provider", cfg.llmProvider)
				}

				// AI remediation planning is Anthropic-only for v1 and toggled
				// independently of AI diagnosis.
				if cfg.enableAIRemediation && cfg.llmProvider == "claude" {
					claudePlanner, plannerErr := planner.NewClaudePlanner(apiKey)
					if plannerErr != nil {
						setupLog.Error(plannerErr, "failed to create AI remediation planner, continuing with rule-based remediation")
					} else {
						claudePlanner.SetModel(cfg.llmModel)
						aiPlanner = claudePlanner
						setupLog.Info("AI remediation planning enabled", "provider", cfg.llmProvider)
					}
				}
			} else {
				setupLog.Info("LLM provider configured but no API key found, AI diagnosis disabled",
					"provider", cfg.llmProvider)
			}
		}

		diagnosisEngine := diagnosis.NewEngine(setupLog, diagnosisProviders...)

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

		// 7. Create remediation proposer with safety guardrails.
		safetyChecker := remediation.NewSafetyChecker(mgr.GetClient(), setupLog)
		var proposerOpts []remediation.ProposerOption
		if aiPlanner != nil {
			proposerOpts = append(proposerOpts, remediation.WithPlanner(aiPlanner))
		}
		proposer := remediation.NewProposer(mgr.GetClient(), safetyChecker, setupLog, proposerOpts...)

		// 8. Start health check reconciler.
		healthReconciler := &controller.HealthCheckReconciler{
			Client:            mgr.GetClient(),
			Detection:         detectionEngine,
			Diagnosis:         diagnosisEngine,
			EventStore:        eventStore,
			EventEmitter:      emitter,
			Proposer:          proposer,
			Logger:            setupLog.WithName("health-check"),
			ReconcileInterval: cfg.healthCheckInterval,
			WebSocket:         wsServer,
		}
		if err := mgr.Add(healthReconciler); err != nil {
			setupLog.Error(err, "unable to start health check reconciler")
			os.Exit(1)
		}

		// 9. Register incident controller.
		if err := (&controller.IncidentController{
			Client: mgr.GetClient(),
			Logger: setupLog.WithName("incident-controller"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create incident controller")
			os.Exit(1)
		}

		// 10. Register remediation controller (executor + verifier + rollback).
		executor := remediation.NewExecutor(mgr.GetClient(), setupLog)
		verifier := remediation.NewVerifier(detectionEngine, mgr.GetClient(), setupLog)
		rollbackHandler := remediation.NewRollback(mgr.GetClient(), setupLog)

		if err := (&controller.RemediationController{
			Client:    mgr.GetClient(),
			Executor:  executor,
			Verifier:  verifier,
			Rollback:  rollbackHandler,
			Logger:    setupLog.WithName("remediation-controller"),
			WebSocket: wsServer,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "RemediationAction")
			os.Exit(1)
		}

		setupLog.Info("Phase 2a/2b health check and remediation enabled",
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

	setupLog.Info("starting manager")
	if err := mgr.Start(signalCtx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
