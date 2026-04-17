package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestBootstrap_CreatesPersonaWhenNoneExists(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("test-uid-123")).
		Build()

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
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
}

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
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Only the pre-existing persona should be present.
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
	// kube-system namespace not present — bootstrap should still create the persona.
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		Build()

	b := &ClusterPersonaBootstrap{Client: cl, Log: logr.Discard()}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	persona := &dorguv1.ClusterPersona{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bootstrapPersonaName}, persona); err != nil {
		t.Fatalf("expected ClusterPersona to be created even without kube-system: %v", err)
	}
	if _, ok := persona.Annotations[annotationClusterUID]; ok {
		t.Errorf("expected no %q annotation when kube-system unavailable", annotationClusterUID)
	}
}

func TestBootstrap_HandlesAlreadyExists(t *testing.T) {
	// Simulate a fake client that returns AlreadyExists on Create.
	// Use an intercepting client wrapper.
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(kubeSystemNS("uid")).
		Build()

	// Pre-create the persona so the fake client returns AlreadyExists on Create.
	existing := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapPersonaName},
		Spec:       dorguv1.ClusterPersonaSpec{Name: bootstrapPersonaName, Environment: "development"},
	}
	_ = cl.Create(context.Background(), existing)

	// Now wipe the list cache but leave the object — simulate the race by calling Start
	// on a bootstrap that will try to create the already-existing persona.
	// The fake client will return AlreadyExists; Start must return nil.
	b := &ClusterPersonaBootstrap{Client: &alwaysAlreadyExistsClient{Client: cl}, Log: logr.Discard()}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start() must return nil on AlreadyExists, got: %v", err)
	}
}

// alwaysAlreadyExistsClient wraps a fake client and returns AlreadyExists on Create.
type alwaysAlreadyExistsClient struct {
	client.Client
}

func (c *alwaysAlreadyExistsClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	// Return empty list to trigger the Create path.
	return nil
}

func (c *alwaysAlreadyExistsClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return errors.NewAlreadyExists(schema.GroupResource{Group: dorguv1.GroupVersion.Group, Resource: "clusterpersonas"}, obj.GetName())
}
