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

package controller

import (
	"github.com/go-logr/logr"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/llm/llmtest"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// countingModel is the model double the cost tests count: every request it
// receives is one billed call.
type countingModel struct{ *llmtest.Client }

func newCountingModel() *countingModel {
	return &countingModel{llmtest.Diagnosing("the memory limit is too low", "cause", llm.ActionResourceAdjustment, 0.7)}
}

func (m *countingModel) calls() int { return m.Calls() }

// wrapPlanner is the planner as cmd wires it: behind the fingerprint cache.
func wrapPlanner(p planner.Planner) planner.Planner {
	return planner.NewCachedPlanner(p, llm.DefaultCacheTTL, logr.Discard())
}
