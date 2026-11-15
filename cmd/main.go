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
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(dorguv1.AddToScheme(scheme))
	utilruntime.Must(dorguv1alpha1.AddToScheme(scheme))
	// CustomResourceDefinitions are read (two of them, by name) by the intake's preflight.
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// preflightTimeout bounds the first run of the intake preflight at startup.
const preflightTimeout = 30 * time.Second

func main() {
	cfg := parseFlags()
	if err := cfg.validate(); err != nil {
		// The logger is not configured yet; flag errors belong on stderr.
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		os.Exit(2)
	}

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
			"metrics-cert-path", cfg.metricsCertPath, "metrics-cert-name", cfg.metricsCertName,
			"metrics-cert-key", cfg.metricsCertKey)

		metricsServerOptions.CertDir = cfg.metricsCertPath
		metricsServerOptions.CertName = cfg.metricsCertName
		metricsServerOptions.KeyName = cfg.metricsCertKey
	}

	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElection,
		LeaderElectionID:       "48ec518d.dorgu.io",
		// Step down voluntarily when the manager ends, so a rolling update does not leave
		// detection without a leader for the lease duration (EKS QA 2026-11-09 F-6: about
		// 30 s after every helm upgrade). Safe here because mgr.Start is the last thing
		// main does: the process exits as soon as it returns, with no cleanup after it.
		// Keep it that way: work added after mgr.Start would run while another replica
		// already holds the lease.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Every component is handed clients.Guarded, which refuses to create or change a
	// RemediationAction; only the intake gets clients.Intake. This is the one call of
	// mgr.GetClient() in this file.
	clients := newClusterClients(mgr.GetClient())

	// Register the spec.nodeName Pod field index in the manager cache before it
	// starts. The resource-saturation checker lists pods-by-node through it; the
	// cache otherwise rejects the field selector with
	// "Index with name field:spec.nodeName does not exist".
	if err := detection.RegisterPodNodeNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		setupLog.Error(err, "unable to register spec.nodeName field index")
		os.Exit(1)
	}

	// The operator's own namespace is what the deny-list must protect. It is read
	// once, here, because both the ClusterPersona controller (which reports an
	// unknown one) and the health pipeline (which refuses to talk to a model
	// without it) depend on the answer.
	operatorNS := operatorNamespace()

	// The proposal intake's preflight: the operator's own identity, the RemediationProposal
	// CRD, and spec.provenance in the RemediationAction CRD. While any of them fails the
	// intake is closed (nothing is converted, planned or proposed) and the operator says
	// so on the ClusterPersona; detection and everything else carry on, and the operator
	// does not crash-loop. The checks run once now, before anything is registered, and
	// then every minute.
	var intakeState intakeSetup
	if cfg.enableHealthCheck {
		intakeState = newIntakeSetup(mgr.GetAPIReader(), operatorNS, setupLog)
		// The username above is a claim from the deployment: have the API server confirm it.
		if preflightClient, icErr := kubernetes.NewForConfig(preflightRESTConfig(restConfig)); icErr != nil {
			setupLog.Error(icErr, "unable to build the client that verifies the operator's identity and permissions; the intake stays closed")
			intakeState.Preflight.Add(intake.StaticCheck("operator-identity-verified",
				intake.ReasonOperatorIdentityUnverified, icErr))
			intakeState.Preflight.Add(intake.StaticCheck("operator-rbac",
				intake.ReasonOperatorRBACUnverified, icErr))
		} else {
			intakeState.verifyIdentity(intake.WhoAmIFromSelfSubjectReview(preflightClient))
			intakeState.verifyRBAC(intake.AccessReviewerFromClientset(preflightClient))
		}
		preflightCtx, cancelPreflight := context.WithTimeout(context.Background(), preflightTimeout)
		intakeState.Preflight.Run(preflightCtx)
		cancelPreflight()
		if err := mgr.Add(intakeState.Preflight); err != nil {
			setupLog.Error(err, "unable to add the intake preflight")
			os.Exit(1)
		}
	}

	// Create discovery client for ClusterPersona controller
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to create discovery client")
		os.Exit(1)
	}

	if err := (&controller.ApplicationPersonaReconciler{
		Client:        clients.Guarded,
		Scheme:        mgr.GetScheme(),
		PrometheusURL: cfg.prometheusURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ApplicationPersona")
		os.Exit(1)
	}

	if cfg.prometheusURL != "" {
		setupLog.Info("Prometheus integration enabled", "url", cfg.prometheusURL)
	}

	clusterPersonaReconciler := &controller.ClusterPersonaReconciler{
		Client:          clients.Guarded,
		Scheme:          mgr.GetScheme(),
		DiscoveryClient: discoveryClient,

		OperatorNamespaceUnknown: operatorNS == "",
	}
	if intakeState.Preflight != nil {
		clusterPersonaReconciler.IntakeReadiness = intakeState.Preflight
	}
	if err := clusterPersonaReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterPersona")
		os.Exit(1)
	}

	// Register ArgoCD watcher if enabled and ArgoCD CRD is present
	if cfg.enableArgoCD {
		if controller.ArgoCDCRDExists(discoveryClient) {
			setupLog.Info("ArgoCD integration enabled, setting up watcher")
			if err := (&controller.ArgoCDWatcher{
				Client:  clients.Guarded,
				Scheme:  mgr.GetScheme(),
				Enabled: true,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Info("ArgoCD watcher setup failed", "error", err.Error())
			}
		} else {
			setupLog.Info("ArgoCD integration disabled: Application CRD (argoproj.io/v1alpha1) not found in cluster")
		}
	}

	// Create a single signal-handler context shared by all components.
	// ctrl.SetupSignalHandler() must only be called once — a second call
	// panics with "close of closed channel".
	signalCtx := ctrl.SetupSignalHandler()

	// Auto-create ClusterPersona on startup if none exists (ArgoCD-style bootstrap).
	if cfg.autoCreateClusterPersona {
		bootstrap := &controller.ClusterPersonaBootstrap{
			Client:         clients.Guarded,
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

		// 1. Assemble the deny-list, diagnosis providers, AI planner and proposer.
		// buildHealthPipeline is the one place they are wired together, and it is
		// built first because detection reads the same deny-list the gate does.
		pipeline := buildHealthPipeline(clients, mgr.GetAPIReader(), cfg, operatorNS, intakeState, setupLog)
		diagnosisEngine := pipeline.DiagnosisEngine

		// 2. Create detection collectors. The pod collector skips the namespaces the
		// gate would refuse (F-13), so platform namespaces raise no incidents.
		nodeCollector := detection.NewNodeCollector(clients.Guarded, setupLog)
		podCollector := newPodCollector(clients.Guarded, pipeline.Gate, setupLog,
			podCollectorOptions(cfg, mgr.GetAPIReader())...)
		resourceCollector := detection.NewResourceCollector(clients.Guarded, setupLog)
		controlPlaneCollector := detection.NewControlPlaneCollector(
			clients.Guarded, clientset.CoreV1().RESTClient(), setupLog,
		)

		collectors := []detection.SignalCollector{
			nodeCollector, podCollector, resourceCollector, controlPlaneCollector,
		}

		// Optional: metrics-server collector, and CPU throttling detection. The
		// throttling collector is always registered: with no metrics source it says
		// so on the ClusterPersona status instead of silently not existing.
		// mc stays a nil interface when the client cannot be built: assigning a nil
		// *Clientset to it would make a non-nil interface that panics on first use.
		var mc metricsclient.Interface
		if metricsClientset, mcErr := metricsclient.NewForConfig(restConfig); mcErr != nil {
			setupLog.Info("metrics-server client creation failed, skipping metrics collectors",
				"error", mcErr.Error())
		} else {
			mc = metricsClientset
		}
		if cfg.enableMetricsServer && mc != nil {
			collectors = append(collectors, detection.NewMetricsCollector(mc, clients.Guarded, setupLog))
		}
		collectors = append(collectors,
			newCPUThrottlingCollector(clients.Guarded, pipeline.Gate, mc, cfg, setupLog))

		// 3. Create detection engine.
		detectionEngine := detection.NewEngine(setupLog, collectors...)
		detectionEngine.SetPersonaCorrelator(
			detection.NewPersonaCorrelator(clients.Guarded, setupLog.WithName("persona-correlator")),
		)

		// 4. Event pipeline: off unless --enable-event-pipeline (nothing reads its
		// output yet). The Kubernetes Event emitter is separate and always on.
		//
		// GetEventRecorderFor is deprecated in favour of GetEventRecorder, but the
		// two return different types: the new events API hands back an
		// events.EventRecorder whose Eventf takes regarding and related objects,
		// where events.NewEmitter and the whole emitter path take a
		// record.EventRecorder. Switching is a real migration of the event
		// pipeline, not a rename. Left for its own change.
		//nolint:staticcheck // SA1019: migrating to the new events API is a separate change
		recorder := mgr.GetEventRecorderFor(events.OperatorEventSource)
		eventPipe := buildEventPipeline(cfg, clients.Guarded, clientset, recorder, setupLog)
		if err := eventPipe.register(mgr); err != nil {
			setupLog.Error(err, "unable to start the event pipeline")
			os.Exit(1)
		}
		if cfg.enableEventPipeline {
			setupLog.Info("event pipeline enabled",
				"retention", cfg.dorguEventRetention, "maxRecords", cfg.dorguEventMaxRecords)
		}

		// 7. Start health check reconciler.
		healthReconciler := &controller.HealthCheckReconciler{
			Client:            clients.Guarded,
			Detection:         detectionEngine,
			Diagnosis:         diagnosisEngine,
			EventStore:        eventPipe.Store,
			EventEmitter:      eventPipe.Emitter,
			Proposer:          pipeline.Proposer,
			Recorder:          recorder,
			Logger:            setupLog.WithName("health-check"),
			ReconcileInterval: cfg.healthCheckInterval,

			APIReader:         mgr.GetAPIReader(),
			RecurrenceCap:     cfg.recurrenceCap,
			RecurrenceWindow:  cfg.recurrenceWindow,
			ResolvedRetention: cfg.resolvedRetention,
		}
		if err := mgr.Add(healthReconciler); err != nil {
			setupLog.Error(err, "unable to start health check reconciler")
			os.Exit(1)
		}

		// 8. Register incident controller.
		if err := (&controller.IncidentController{
			Client: clients.Guarded,
			Logger: setupLog.WithName("incident-controller"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create incident controller")
			os.Exit(1)
		}

		// 8b. Register the proposal reconciler: the one intake for every
		// RemediationProposal, and the only code that creates a RemediationAction.
		// It is registered as soon as the RemediationProposal CRD exists, which is
		// now or, if the CRD has not been applied yet, whenever it is (a manager
		// whose proposal cache cannot sync exits, so the operator must not register
		// the controller early).
		proposals := &lateRegistration{
			log: setupLog,
			register: func() error {
				return (&controller.ProposalReconciler{
					Client:    clients.Guarded,
					Processor: pipeline.Intake,
				}).SetupWithManager(mgr)
			},
		}
		intakeState.Preflight.Subscribe(proposals.onChecks)
		proposals.onChecks(intakeState.Preflight.Failures())

		// 9. Register remediation controller (executor + verifier + rollback).
		executor := remediation.NewExecutor(clients.Guarded, pipeline.Gate, setupLog)
		verifier := remediation.NewVerifier(detectionEngine, clients.Guarded, setupLog)
		rollbackHandler := remediation.NewRollback(clients.Guarded, setupLog)

		if err := (&controller.RemediationController{
			Client:   clients.Guarded,
			Gate:     pipeline.Gate,
			Executor: executor,
			Verifier: verifier,
			Rollback: rollbackHandler,
			Recorder: recorder,
			Logger:   setupLog.WithName("remediation-controller"),

			// With the admission policies missing (or the operator's permissions partial)
			// no ticket is issued; a nil preflight (no health check) blocks nothing.
			TicketGuard: ticketGuardFor(intakeState),

			PendingTTL: cfg.remediationPendingTTL,
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

// operatorNamespace returns the namespace the deny-list must protect as
// Dorgu's own, and says so when it cannot be determined.
func operatorNamespace() string {
	ns := gate.OperatorNamespace()
	if ns == "" {
		setupLog.Info("operator namespace unknown: set " + gate.OperatorNamespaceEnv +
			"; the deny-list protects kube-system and --safety-excluded-namespaces only, " +
			"so AI diagnosis and AI planning stay off")
		return ""
	}
	setupLog.Info("deny-list protects the operator namespace", "namespace", ns)
	return ns
}
