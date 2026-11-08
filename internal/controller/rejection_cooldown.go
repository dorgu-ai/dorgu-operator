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
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

const (
	// RejectionCooldown is how long a rejection holds before dorgu will propose
	// the same fix for the same incident again.
	//
	// Rejecting used to buy about 30 seconds: the health check re-proposed on the
	// next cycle, and with the AI planner configured every re-proposal is another
	// billed call, so declining cost the user money (F-07). An hour is long
	// enough that saying no is respected, short enough that a problem still
	// present an hour later gets raised again.
	RejectionCooldown = gate.RejectionCooldown

	// ConditionRejected records that a human declined a RemediationAction, and
	// when. The phase alone cannot carry the timestamp.
	ConditionRejected = gate.ConditionRejected

	// ReasonUserRejected is the ConditionRejected reason for a human decision.
	ReasonUserRejected = "UserRejected"
)
