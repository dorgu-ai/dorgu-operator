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

// F-07a: detection used to be off by default, so following the page called
// "Installation" produced an operator that never detected anything, with nothing
// on the page hinting why. The self-healing loop is the product, so it is on out
// of the box; AI stays opt-in because inference costs money and sends data out.
package chart

import "testing"

// Detection is on with default values.
func TestHealthCheckEnabledByDefault(t *testing.T) {
	out := helmTemplate(t)
	mustContain(t, out, "--enable-health-check=true", "default install")
	mustContain(t, out, "--health-check-interval=60s", "default interval")
}

// The flag wiring still lets an operator turn it off.
func TestHealthCheckCanBeDisabled(t *testing.T) {
	out := helmTemplate(t, "healthCheck.enabled=false")
	mustContain(t, out, "--enable-health-check=false", "explicit opt out")
	mustNotContain(t, out, "--enable-health-check=true", "opt out must not leave detection on")
	mustNotContain(t, out, "--health-check-interval", "no interval when detection is off")
}

// AI is not implied by detection: a default install spends nothing on inference.
func TestDefaultInstallDoesNotEnableAI(t *testing.T) {
	out := helmTemplate(t)
	mustNotContain(t, out, "--llm-provider", "AI provider must stay opt-in")
	mustNotContain(t, out, "--enable-ai-remediation", "AI remediation must stay opt-in")
}

// A custom interval reaches the operator.
func TestHealthCheckIntervalOverride(t *testing.T) {
	out := helmTemplate(t, "healthCheck.interval=30s")
	mustContain(t, out, "--enable-health-check=true", "detection stays on")
	mustContain(t, out, "--health-check-interval=30s", "custom interval")
}
