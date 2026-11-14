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

package main

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

// clusterClients are the clients the operator's components are handed.
//
// The proposal intake is the only code that creates a RemediationAction (P2-D1), and
// that is enforced here, structurally: every component gets Guarded, a client that
// refuses to create, update, patch or apply a RemediationAction in any form, and
// only the intake Processor gets Intake, the manager's own client. Do not call
// mgr.GetClient() anywhere else (a test reads main.go to make sure).
type clusterClients struct {
	// Guarded is for everything except the intake.
	Guarded client.Client

	// Intake is the raw client, for intake.Processor alone.
	Intake client.Client
}

// newClusterClients wraps the manager's client.
func newClusterClients(raw client.Client) clusterClients {
	return clusterClients{Guarded: intake.GuardActionWrites(raw), Intake: raw}
}
