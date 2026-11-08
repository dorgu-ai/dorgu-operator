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

package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// F-5: the unauthenticated WebSocket server is gone. Nothing websocket-shaped
// renders, whether or not an old values file still sets it.
func TestNoWebSocketRendered(t *testing.T) {
	out := helmTemplate(t)
	mustNotContain(t, out, "websocket", "no websocket Service, port or flag")
	mustNotContain(t, out, "9090", "no websocket port")
}

// An upgrade with an old values file must not fail: websocket.* is now an unknown
// key, which Helm ignores. The chart renders, and still renders nothing for it.
func TestOldWebSocketValuesAreIgnored(t *testing.T) {
	out := helmTemplate(t,
		"websocket.enabled=true",
		"websocket.port=9191",
		"websocket.service.type=LoadBalancer",
		"websocket.service.port=9191",
	)
	mustNotContain(t, out, "websocket", "removed values must not render anything")
	mustNotContain(t, out, "LoadBalancer", "a removed Service type must not leak out")
	mustNotContain(t, out, "9191", "a removed port must not leak out")
}

func TestOldValuesFileStillInstalls(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
	values := filepath.Join(t.TempDir(), "old-values.yaml")
	old := "websocket:\n  enabled: true\n  port: 9090\n  service:\n    type: ClusterIP\n    port: 9090\n"
	if err := os.WriteFile(values, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("helm", "template", ".", "-f", values).CombinedOutput(); err != nil {
		t.Fatalf("a pre-removal values file must still render: %v\n%s", err, out)
	}
}

// 1.15: the event pipeline is off unless asked for.
func TestEventPipelineOffByDefault(t *testing.T) {
	out := helmTemplate(t)
	mustContain(t, out, "--enable-event-pipeline=false", "the pod spec states the pipeline is off")
	mustNotContain(t, out, "--dorgu-event-retention", "retention means nothing with the pipeline off")
	mustNotContain(t, out, "--dorgu-event-max-records", "the record cap means nothing with the pipeline off")
}

func TestEventPipelineCanBeEnabled(t *testing.T) {
	out := helmTemplate(t, "eventPipeline.enabled=true", "dorguEvents.retention=6h", "dorguEvents.maxRecords=500")
	mustContain(t, out, "--enable-event-pipeline=true", "explicit opt in")
	mustContain(t, out, "--dorgu-event-retention=6h", "retention reaches the operator")
	mustContain(t, out, "--dorgu-event-max-records=500", "the cap reaches the operator")
}
