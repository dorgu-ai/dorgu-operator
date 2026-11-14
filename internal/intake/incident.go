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

package intake

import (
	"context"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// IncidentOpener opens the IncidentMemory a proposal is answered against when the
// proposal names none. Opening is idempotent: asking again for the same proposal
// returns the incident it opened the first time.
type IncidentOpener interface {
	Open(ctx context.Context, proposal *v1alpha1.RemediationProposal) (*dorguv1.IncidentMemory, error)
}
