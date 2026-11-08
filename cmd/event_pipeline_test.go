package main

import (
	"flag"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
)

// recordingManager remembers which runnables were added to it.
type recordingManager struct{ runnables []manager.Runnable }

func (m *recordingManager) Add(r manager.Runnable) error {
	m.runnables = append(m.runnables, r)
	return nil
}

func buildEventPipelineForTest(t *testing.T, cfg operatorConfig) eventPipeline {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
	return buildEventPipeline(cfg, c, fake.NewClientset(), record.NewFakeRecorder(8), logr.Discard())
}

// 1.15: the event pipeline feeds nothing, so it is not started unless asked for.
func TestEventPipeline_NotStartedByDefault(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), nil)
	require.NoError(t, err)
	require.False(t, cfg.enableEventPipeline, "the flag must default to off")

	p := buildEventPipelineForTest(t, cfg)
	assert.Nil(t, p.Store, "no DorguEvent store: nothing would prune what it wrote")
	assert.Nil(t, p.Watcher)
	assert.Nil(t, p.Cleaner)
	assert.NotNil(t, p.Emitter, "Kubernetes Events on incidents stay on; they are not the pipeline")

	m := &recordingManager{}
	require.NoError(t, p.register(m))
	assert.Empty(t, m.runnables, "no runnable may be started with the flag off")
}

func TestEventPipeline_StartedWhenEnabled(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), []string{"--enable-event-pipeline=true"})
	require.NoError(t, err)
	require.True(t, cfg.enableEventPipeline)

	p := buildEventPipelineForTest(t, cfg)
	assert.NotNil(t, p.Store)
	assert.NotNil(t, p.Emitter)
	require.NotNil(t, p.Watcher)
	require.NotNil(t, p.Cleaner)

	m := &recordingManager{}
	require.NoError(t, p.register(m))
	require.Len(t, m.runnables, 2, "the watcher and the cleaner")
	assert.IsType(t, &events.Watcher{}, m.runnables[0])
	assert.IsType(t, &events.Cleaner{}, m.runnables[1])
}

// F-5: the WebSocket server is gone with its flags. A flag that parses and does
// nothing would leave an install believing it had a server.
func TestRemovedWebSocketFlagsAreRejected(t *testing.T) {
	for _, arg := range []string{"--enable-websocket=true", "--websocket-addr=:9090"} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(discard{})
		_, err := parseFlagSet(fs, []string{arg})
		assert.Error(t, err, arg)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// failingManager refuses every runnable.
type failingManager struct{ err error }

func (m failingManager) Add(manager.Runnable) error { return m.err }

func TestEventPipeline_RegisterReportsAManagerError(t *testing.T) {
	cfg := operatorConfig{enableEventPipeline: true}
	p := buildEventPipelineForTest(t, cfg)

	require.ErrorIs(t, p.register(failingManager{err: assert.AnError}), assert.AnError)

	// The cleaner is the second runnable; a manager that accepts the watcher and
	// refuses the cleaner must still surface the error.
	m := &refuseSecond{err: assert.AnError}
	require.ErrorIs(t, p.register(m), assert.AnError)
	assert.Equal(t, 2, m.calls)
}

type refuseSecond struct {
	calls int
	err   error
}

func (m *refuseSecond) Add(manager.Runnable) error {
	m.calls++
	if m.calls == 2 {
		return m.err
	}
	return nil
}
