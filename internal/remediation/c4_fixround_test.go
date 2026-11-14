package remediation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// C4 fix round (review F-2, F-3, F-5, F-11 on PR 64).

// ---------------------------------------------------------------------------
// F-2 / F-5: the verifier cannot read a CPU fix Healthy while the CPU source is blind,
// and a blind CPU source says nothing about any other fix.
// ---------------------------------------------------------------------------

type cpuMetrics struct {
	present bool
	listErr error
}

func (m cpuMetrics) Available(context.Context) (bool, error) { return m.present, nil }
func (m cpuMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	return nil, m.listErr
}

func cpuCollectorWith(t *testing.T, opts ...detection.CPUThrottlingOption) *detection.CPUThrottlingCollector {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	return detection.NewCPUThrottlingCollector(k, logr.Discard(),
		append([]detection.CPUThrottlingOption{detection.WithMinCycleSpacing(0)}, opts...)...)
}

func verifierOver(t *testing.T, signal string, collectors ...detection.SignalCollector) *Verifier {
	t.Helper()
	incident := testIncident()
	incident.Spec.Detection.Signal = signal
	k := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).WithObjects(incident).Build()
	return NewVerifier(detection.NewEngine(logr.Discard(), collectors...), k, logr.Discard())
}

func TestVerifier_CPUFixIsUnknownWhileTheCPUSourceIsBlind(t *testing.T) {
	tests := []struct {
		name string
		opts []detection.CPUThrottlingOption
	}{
		{"no source after a restart (the reviewer's F-2)", nil},
		{"metrics.k8s.io not served", []detection.CPUThrottlingOption{detection.WithMetricsSource(cpuMetrics{present: false})}},
		{"metrics.k8s.io unreadable", []detection.CPUThrottlingOption{detection.WithMetricsSource(cpuMetrics{present: true, listErr: errors.New("503")})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := verifierOver(t, string(detection.SignalCPUThrottling), cpuCollectorWith(t, tt.opts...))
			result, _ := v.Verify(context.Background(), testRemediationAction())
			assert.Equal(t, VerificationUnknown, result, "nobody could look at CPU, so a CPU fix has not been shown to work")
		})
	}
}

func TestVerifier_CPUFixIsHealthyWhenTheCPUSourceCanLookAndSeesNothing(t *testing.T) {
	v := verifierOver(t, string(detection.SignalCPUThrottling),
		cpuCollectorWith(t, detection.WithMetricsSource(cpuMetrics{present: true})))
	result, err := v.Verify(context.Background(), testRemediationAction())
	require.NoError(t, err)
	assert.Equal(t, VerificationHealthy, result)
}

func TestVerifier_AnOOMFixStillVerifiesWhileTheCPUSourceIsBlind(t *testing.T) {
	for name, opts := range map[string][]detection.CPUThrottlingOption{
		"no source":  nil,
		"unreadable": {detection.WithMetricsSource(cpuMetrics{present: true, listErr: errors.New("503")})},
	} {
		t.Run(name, func(t *testing.T) {
			v := verifierOver(t, "OOMKilled", cpuCollectorWith(t, opts...))
			result, err := v.Verify(context.Background(), testRemediationAction())
			require.NoError(t, err, "a CPU metrics outage must not roll back a memory fix")
			assert.Equal(t, VerificationHealthy, result)
		})
	}
}

func TestVerifier_ABlindPodCollectorStillMakesEveryVerificationUnknown(t *testing.T) {
	// B2b for the collectors that read pods and workloads.
	v := verifierOver(t, "OOMKilled",
		cpuCollectorWith(t, detection.WithMetricsSource(cpuMetrics{present: true})),
		&mockCollector{err: errors.New("pod list failed")})
	result, err := v.Verify(context.Background(), testRemediationAction())
	require.Error(t, err)
	assert.Equal(t, VerificationUnknown, result)
}

// ---------------------------------------------------------------------------
// F-3: the proposal is sized from the throttled container
// ---------------------------------------------------------------------------

func twoContainerDeployment() (*dorguv1.ApplicationPersona, client.Object) {
	persona := personaWithLimits("my-app", "2", "256Mi")
	deploy := liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "256Mi"}, nil)
	deploy.Spec.Template.Spec.Containers = append(deploy.Spec.Template.Spec.Containers, corev1.Container{
		Name: "sidecar", Image: "envoy:1",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
	})
	return persona, deploy
}

func proposeFor(t *testing.T, container string) *ProposalResult {
	t.Helper()
	persona, deploy := twoContainerDeployment()
	diag := newCPUThrottlingDiagnosis(detection.SeverityWarning)
	diag.Contributing[0].Signal.Metadata["container"] = container
	incident := newTestIncident(defaultNamespace, "cpu-throttle", "my-app", "CPUThrottling")
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	result, err := NewProposer(c, gate.New(c, testLogger()), testLogger()).Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	return result
}

func TestProposer_CPUThrottling_ThrottledSidecarIsNotSizedFromTheMainContainer(t *testing.T) {
	result := proposeFor(t, "sidecar")
	require.False(t, result.Proposed,
		"the sidecar is throttled; raising the main container's 2 CPU limit spends an approval on the wrong container")
	assert.Equal(t, "container-mismatch", result.Reason)
	assert.Contains(t, result.SkipReason, `"sidecar"`, "the advisory names the throttled container")
	assert.Contains(t, result.SkipReason, `"app"`, "and the one the persona's resources block describes")
}

func TestProposer_CPUThrottling_ThrottledMainContainerIsProposed(t *testing.T) {
	result := proposeFor(t, "app")
	require.True(t, result.Proposed, "skip: %s", result.SkipReason)
	assert.Equal(t, "2500m", patchLimit(t, planned(result.Action).Patch.Raw, "cpu"))
}

func TestProposer_CPUThrottling_NoContainerInTheSignalKeepsTheOldBehaviour(t *testing.T) {
	result := proposeFor(t, "")
	require.True(t, result.Proposed, "skip: %s", result.SkipReason)
}

// ---------------------------------------------------------------------------
// F-11: critical comes from a real signal through the real rule, not a hand-built diagnosis
// ---------------------------------------------------------------------------

type promSamples []detection.ThrottleSample

func (p promSamples) Query(context.Context) ([]detection.ThrottleSample, error) { return p, nil }

func collectorSignal(t *testing.T, ratio float64) detection.Signal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-abc", Namespace: defaultNamespace},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")}},
		}}},
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	c := detection.NewCPUThrottlingCollector(k, logr.Discard(), detection.WithMinCycleSpacing(0),
		detection.WithThrottleSource(promSamples{{Namespace: defaultNamespace, Pod: "my-app-abc", Container: "app", Ratio: ratio}}))
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, signals, 1)
	signals[0].PersonaRef = &dorguv1.PersonaReference{Kind: kindApplicationPersona, Name: "my-app", Namespace: defaultNamespace}
	signals[0].DetectedAt = time.Now()
	return signals[0]
}

func TestProposer_CPUThrottling_CriticalFromAThrottledRatioOfHalfOrMore(t *testing.T) {
	tests := []struct {
		ratio float64
		want  string
	}{
		{0.30, "500m"}, // warning: +25%
		{0.50, "600m"}, // critical: +50%
		{0.95, "600m"},
	}
	for _, tt := range tests {
		sig := collectorSignal(t, tt.ratio)
		diags, err := diagnosis.NewRuleBasedProvider(logr.Discard()).Diagnose(context.Background(), []detection.Signal{sig})
		require.NoError(t, err)
		require.Len(t, diags, 1)

		persona := personaWithLimits("my-app", "50m", "256Mi")
		deploy := liveDeployment("my-app", map[corev1.ResourceName]string{corev1.ResourceCPU: "400m", corev1.ResourceMemory: "256Mi"}, nil)
		incident := newTestIncident(defaultNamespace, "cpu-throttle", "my-app", "CPUThrottling")
		c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithRuntimeObjects(persona, deploy).
			WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
		result, err := NewProposer(c, gate.New(c, testLogger()), testLogger()).Propose(context.Background(), diags[0], incident)
		require.NoError(t, err)
		require.True(t, result.Proposed, "ratio %.2f: %s", tt.ratio, result.SkipReason)
		assert.Equal(t, tt.want, patchLimit(t, planned(result.Action).Patch.Raw, "cpu"), "ratio %.2f", tt.ratio)
	}
}
