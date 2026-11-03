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
	"os/exec"
	"testing"
)

// The Deployment webhook is gone (0.7). Nothing webhook-shaped renders, and a
// leftover webhook.enabled=true fails the install instead of being ignored.
func TestNoWebhookRendered(t *testing.T) {
	out := helmTemplate(t)
	mustNotContain(t, out, "ValidatingWebhookConfiguration", "no webhook configuration")
	mustNotContain(t, out, "--enable-webhook", "no webhook flag")
	mustNotContain(t, out, "webhook-certs", "no webhook cert volume")
}

func TestRemovedWebhookValueFailsLoudly(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", ".", "--set", "webhook.enabled=true").CombinedOutput()
	if err == nil {
		t.Fatal("webhook.enabled=true rendered; expected the chart to fail")
	}
	mustContain(t, string(out), "webhook.enabled was removed", "explains the removal")
}
