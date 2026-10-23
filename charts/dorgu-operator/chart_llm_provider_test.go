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

// The key env var must match llm.provider. The chart used to inject
// ANTHROPIC_API_KEY whatever the provider, so llm.provider=gemini started an
// operator that read GEMINI_API_KEY, found nothing, and ran without AI.
package chart

import (
	"os/exec"
	"testing"
)

func TestGeminiProviderGetsGeminiEnv(t *testing.T) {
	out := helmTemplate(t, "llm.provider=gemini", "llm.existingSecret=dorgu-llm")
	mustContain(t, out, "--llm-provider=gemini", "provider arg")
	mustContain(t, out, "name: GEMINI_API_KEY", "gemini env var")
	mustContain(t, out, "key: GEMINI_API_KEY", "per-provider default secret key")
	mustNotContain(t, out, "ANTHROPIC_API_KEY", "no Anthropic env for gemini")
}

func TestGeminiChartManagedSecret(t *testing.T) {
	out := helmTemplate(t, "llm.provider=gemini", "llm.apiKey=AIza-test", "llm.createSecret=true")
	mustContain(t, out, `GEMINI_API_KEY: "AIza-test"`, "stringData uses the gemini key name")
	mustContain(t, out, "name: GEMINI_API_KEY", "env var")
}

// Backward compatible: an explicit existingSecretKey still wins for either provider.
func TestExplicitSecretKeyStillWins(t *testing.T) {
	out := helmTemplate(t, "llm.provider=gemini", "llm.existingSecret=s", "llm.existingSecretKey=MY_KEY")
	mustContain(t, out, "name: GEMINI_API_KEY", "env var follows the provider")
	mustContain(t, out, "key: MY_KEY", "explicit key name")
}

func TestUnknownProviderFails(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", ".", "--set", "llm.provider=openai").CombinedOutput()
	if err == nil {
		t.Fatal("llm.provider=openai rendered; expected the chart to refuse it")
	}
	mustContain(t, string(out), "llm.provider", "names the bad value")
}
