package detection

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func runtimeSchemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}
func newFakeForTest(s *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().WithScheme(s).Build()
}
func testLogger() logr.Logger                { return zap.New(zap.UseDevMode(true)) }
func ctxBG() context.Context                 { return context.Background() }
func keyOf(o client.Object) client.ObjectKey { return client.ObjectKeyFromObject(o) }
