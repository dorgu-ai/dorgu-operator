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

package events

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestEmitter_EmitWarningEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)
	logger := zap.New(zap.UseDevMode(true))
	emitter := NewEmitter(fakeRecorder, logger)

	event := &InternalEvent{
		ID:       "test-1",
		Severity: SeverityCritical,
		Category: CategoryHealth,
		Source:   "kubelet",
		Message:  "Container was OOM killed",
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      "Pod",
			Name:      "api-server-xyz",
			Namespace: "production",
		},
		EventTime: time.Now(),
	}

	err := emitter.Emit(context.Background(), event)
	require.NoError(t, err)

	select {
	case recorded := <-fakeRecorder.Events:
		assert.Contains(t, recorded, "Warning")
		assert.Contains(t, recorded, "DorguDetected")
		assert.Contains(t, recorded, "[dorgu] critical: Container was OOM killed")
	default:
		t.Fatal("expected an event to be recorded")
	}
}

func TestEmitter_EmitNormalEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)
	logger := zap.New(zap.UseDevMode(true))
	emitter := NewEmitter(fakeRecorder, logger)

	event := &InternalEvent{
		ID:       "test-2",
		Severity: SeverityInfo,
		Category: CategoryScaling,
		Source:   "deployment-controller",
		Message:  "Scaled up replica set to 3",
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      kindDeployment,
			Name:      "web-app",
			Namespace: "default",
		},
		EventTime: time.Now(),
	}

	err := emitter.Emit(context.Background(), event)
	require.NoError(t, err)

	select {
	case recorded := <-fakeRecorder.Events:
		assert.Contains(t, recorded, "Normal")
		assert.Contains(t, recorded, "DorguDetected")
		assert.Contains(t, recorded, "[dorgu] info: Scaled up replica set to 3")
	default:
		t.Fatal("expected an event to be recorded")
	}
}

func TestEmitter_EmitWarningForWarningSeverity(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)
	logger := zap.New(zap.UseDevMode(true))
	emitter := NewEmitter(fakeRecorder, logger)

	event := &InternalEvent{
		ID:       "test-3",
		Severity: SeverityWarning,
		Category: CategoryNode,
		Source:   "kubelet",
		Message:  "Node has memory pressure",
		InvolvedObject: dorguv1.ResourceReference{
			Kind: "Node",
			Name: "worker-1",
		},
		EventTime: time.Now(),
	}

	err := emitter.Emit(context.Background(), event)
	require.NoError(t, err)

	select {
	case recorded := <-fakeRecorder.Events:
		assert.Contains(t, recorded, "Warning")
		assert.Contains(t, recorded, "[dorgu] warning:")
	default:
		t.Fatal("expected an event to be recorded")
	}
}

func TestEmitter_NilEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)
	logger := zap.New(zap.UseDevMode(true))
	emitter := NewEmitter(fakeRecorder, logger)

	err := emitter.Emit(context.Background(), nil)
	assert.NoError(t, err)

	select {
	case <-fakeRecorder.Events:
		t.Fatal("no event should be recorded for nil input")
	default:
		// Expected — no event recorded.
	}
}

func TestEmitter_ImplementsInterface(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)
	logger := zap.New(zap.UseDevMode(true))
	var e Emitter = NewEmitter(fakeRecorder, logger)
	assert.NotNil(t, e)
}
