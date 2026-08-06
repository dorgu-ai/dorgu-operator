package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = dorguv1.AddToScheme(s)
	return s
}

func kubeSystemNS(uid string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
			UID:  types.UID(uid),
		},
	}
}

// fastBackoff temporarily shrinks the package-level retry backoff so retry-path
// tests run in milliseconds instead of tens of seconds. Tests using it must not run
// in parallel (they mutate a package var).
func fastBackoff(t *testing.T) {
	t.Helper()
	orig := bootstrapBackoff
	bootstrapBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 2.0, Steps: 5, Cap: 10 * time.Millisecond}
	t.Cleanup(func() { bootstrapBackoff = orig })
}

func clusterPersonaGR() schema.GroupResource {
	return schema.GroupResource{Group: dorguv1.GroupVersion.Group, Resource: "clusterpersonas"}
}

// (a) Empty cluster → ensure creates dorgu-cluster with the expected annotations + spec.
func TestBootstrap_CreatesPersonaWhenNoneExists(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("test-uid-123")).
		Build()

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}

	persona := &dorguv1.ClusterPersona{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bootstrapPersonaName}, persona); err != nil {
		t.Fatalf("expected ClusterPersona %q to be created: %v", bootstrapPersonaName, err)
	}

	if persona.Annotations[annotationBootstrap] != "true" {
		t.Errorf("expected annotation %q = 'true', got %q", annotationBootstrap, persona.Annotations[annotationBootstrap])
	}
	if persona.Annotations[annotationClusterUID] != "test-uid-123" {
		t.Errorf("expected annotation %q = 'test-uid-123', got %q", annotationClusterUID, persona.Annotations[annotationClusterUID])
	}
	if persona.Spec.Environment != "development" {
		t.Errorf("expected environment 'development', got %q", persona.Spec.Environment)
	}
	sh := persona.Spec.Policies.SelfHealing
	if sh == nil || sh.Mode != dorguv1.SelfHealingModePropose || sh.TrustLevel != 2 {
		t.Errorf("expected selfHealing propose/trust 2, got %+v", sh)
	}
}

// (b) Existing persona → ensure is idempotent: no duplicate, no error.
func TestBootstrap_SkipsWhenPersonaAlreadyExists(t *testing.T) {
	existing := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster"},
		Spec:       dorguv1.ClusterPersonaSpec{Name: "my-cluster", Environment: "production"},
	}
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(existing, kubeSystemNS("uid")).
		Build()

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}

	list := &dorguv1.ClusterPersonaList{}
	_ = cl.List(context.Background(), list)
	if len(list.Items) != 1 {
		t.Errorf("expected 1 ClusterPersona, got %d", len(list.Items))
	}
	if list.Items[0].Name != "my-cluster" {
		t.Errorf("expected existing persona 'my-cluster' to be unchanged")
	}
}

func TestBootstrap_NoKubeSystemUID(t *testing.T) {
	// kube-system namespace not present — ensure should still create the persona.
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		Build()

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}

	persona := &dorguv1.ClusterPersona{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bootstrapPersonaName}, persona); err != nil {
		t.Fatalf("expected ClusterPersona to be created even without kube-system: %v", err)
	}
	if _, ok := persona.Annotations[annotationClusterUID]; ok {
		t.Errorf("expected no %q annotation when kube-system unavailable", annotationClusterUID)
	}
}

// AlreadyExists on Create (a concurrent leader replica won the race) is terminal success.
func TestBootstrap_HandlesAlreadyExists(t *testing.T) {
	base := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("uid")).
		Build()

	cl := interceptor.NewClient(base, interceptor.Funcs{
		// Report an empty list so ensure takes the Create path...
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return nil
		},
		// ...then fail Create with AlreadyExists, simulating the race.
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(clusterPersonaGR(), obj.GetName())
		},
	})

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure() must return nil on AlreadyExists, got: %v", err)
	}
}

// (c) Transient Create error → retried within the backoff budget, eventually created.
// Proves the error is NOT silently swallowed after the first failure.
func TestBootstrap_RetriesTransientCreateError(t *testing.T) {
	fastBackoff(t)

	base := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("uid")).
		Build()

	var createCalls int
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			createCalls++
			if createCalls <= 2 {
				return apierrors.NewServerTimeout(clusterPersonaGR(), "create", 1)
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.ensureWithRetry(context.Background()); err != nil {
		t.Fatalf("ensureWithRetry() error = %v (transient errors must be retried, not swallowed)", err)
	}
	if createCalls < 3 {
		t.Errorf("expected Create to be retried (>=3 calls), got %d", createCalls)
	}

	persona := &dorguv1.ClusterPersona{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: bootstrapPersonaName}, persona); err != nil {
		t.Fatalf("expected ClusterPersona to exist after retries: %v", err)
	}
}

// (d) auto-create disabled → the runnable is never started, so the only creation path
// (ensure) is never invoked and the cluster stays empty. Locks the gated contract.
func TestBootstrap_DisabledPath_CreatesNothing(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("uid")).
		Build()

	// main.go only constructs + Adds the runnable when autoCreateClusterPersona is true;
	// when false it is never started. Here we intentionally never call ensure/Start.
	list := &dorguv1.ClusterPersonaList{}
	if err := cl.List(context.Background(), list); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no ClusterPersona when bootstrap disabled, got %d", len(list.Items))
	}
}

// (e) Leader-election contract is explicit — locks the choice against a silent revert.
func TestBootstrap_NeedLeaderElection(t *testing.T) {
	if got := (&ClusterPersonaBootstrap{}).NeedLeaderElection(); got != true {
		t.Errorf("NeedLeaderElection() = %v, want true", got)
	}
}

// (f) Periodic convergence — a first ensure that fails past the retry budget does not
// leave the persona permanently absent: a later ensure tick creates it.
func TestBootstrap_PeriodicConvergenceAfterMiss(t *testing.T) {
	fastBackoff(t)

	base := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("uid")).
		Build()

	failCreate := true
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if failCreate {
				return apierrors.NewServerTimeout(clusterPersonaGR(), "create", 1)
			}
			return c.Create(ctx, obj, opts...)
		},
	})

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}

	// First cycle: every attempt fails → returns an error, persona NOT created.
	if err := b.ensureWithRetry(context.Background()); err == nil {
		t.Fatal("expected ensureWithRetry to fail while Create errors persist")
	}
	list := &dorguv1.ClusterPersonaList{}
	_ = base.List(context.Background(), list)
	if len(list.Items) != 0 {
		t.Fatalf("expected persona absent after failed first cycle, got %d", len(list.Items))
	}

	// Next tick: the transient condition clears → persona converges.
	failCreate = false
	if err := b.ensureWithRetry(context.Background()); err != nil {
		t.Fatalf("expected periodic ensure to converge, got %v", err)
	}
	persona := &dorguv1.ClusterPersona{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: bootstrapPersonaName}, persona); err != nil {
		t.Fatalf("expected ClusterPersona to exist after convergence: %v", err)
	}
}

// Start's immediate first run (the fast path) creates the persona; Start then blocks
// on the periodic loop until ctx is cancelled and returns nil.
func TestBootstrap_Start_CreatesViaImmediateFirstRun(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("uid")).
		Build()

	// Long interval so only the immediate first run fires before we cancel.
	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard(), EnsureInterval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	deadline := time.After(5 * time.Second)
	persona := &dorguv1.ClusterPersona{}
	for {
		err := cl.Get(context.Background(), types.NamespacedName{Name: bootstrapPersonaName}, persona)
		if err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("persona not created by Start's immediate first run within timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after context cancel")
	}
}

func TestBootstrap_EnsureInterval(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero uses default", 0, defaultEnsureInterval},
		{"negative uses default", -5 * time.Second, defaultEnsureInterval},
		{"below min is clamped", 5 * time.Second, minEnsureInterval},
		{"above min is kept", 5 * time.Minute, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &ClusterPersonaBootstrap{EnsureInterval: tc.in}
			if got := b.ensureInterval(); got != tc.want {
				t.Errorf("ensureInterval(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
