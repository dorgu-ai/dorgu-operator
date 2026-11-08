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

// The deny-list must protect the namespace Dorgu runs in (OP-03). The binary
// learns it from POD_NAMESPACE, which only the downward API can supply
// reliably, so every install renders it.
package chart

import "testing"

func TestPodNamespaceFromDownwardAPI(t *testing.T) {
	out := helmTemplate(t)
	mustContain(t, out, "name: POD_NAMESPACE", "operator namespace env")
	mustContain(t, out, "fieldPath: metadata.namespace", "sourced from the downward API")
}

func TestSafetyExcludedNamespaces(t *testing.T) {
	out := helmTemplate(t)
	mustNotContain(t, out, "--safety-excluded-namespaces", "no flag without extra namespaces")

	custom := helmTemplate(t, "safety.excludedNamespaces={payments,billing}")
	mustContain(t, custom, "--safety-excluded-namespaces=payments,billing", "extra namespaces reach the flag")
}

func TestRemediationPendingTTL(t *testing.T) {
	out := helmTemplate(t)
	mustContain(t, out, "--remediation-pending-ttl=24h", "the default expiry is rendered explicitly")

	custom := helmTemplate(t, "remediation.pendingTTL=2h")
	mustContain(t, custom, "--remediation-pending-ttl=2h", "a configured TTL reaches the flag")
	mustNotContain(t, custom, "--remediation-pending-ttl=24h", "the override replaces the default")
}

func TestLLMCacheTTL(t *testing.T) {
	off := helmTemplate(t)
	mustNotContain(t, off, "--llm-cache-ttl", "no AI provider, no AI flags")

	out := helmTemplate(t, "llm.provider=claude")
	mustContain(t, out, "--llm-cache-ttl=30m", "the default is rendered explicitly when AI is on")

	custom := helmTemplate(t, "llm.provider=claude", "llm.cacheTTL=5m")
	mustContain(t, custom, "--llm-cache-ttl=5m", "a configured TTL reaches the flag")
	mustNotContain(t, custom, "--llm-cache-ttl=30m", "the override replaces the default")

	disabled := helmTemplate(t, "llm.provider=claude", "llm.cacheTTL=0")
	mustContain(t, disabled, "--llm-cache-ttl=0", "0 disables the cache")
}
