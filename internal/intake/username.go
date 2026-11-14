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
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ServiceAccountEnv is the env var the chart and kustomize manifests set from the
// downward API (spec.serviceAccountName): the ServiceAccount the operator runs as.
const ServiceAccountEnv = "OPERATOR_SERVICE_ACCOUNT"

// ErrOperatorIdentity is the error OperatorUsername wraps when it cannot say who the
// operator is.
var ErrOperatorIdentity = errors.New("the operator does not know its own ServiceAccount")

// OperatorUsername is the Kubernetes username of the operator's ServiceAccount,
// "system:serviceaccount:<namespace>:<name>": the identity Dorgu's own planner
// proposes as (P2-D3). namespace is the operator's namespace.
//
// It never guesses. When either part is missing or is not a valid Kubernetes name it
// returns an error (wrapping ErrOperatorIdentity) and no username: a made-up one
// would be recorded as every proposal's and action's proposer, and any later
// "the approver is not the proposer" comparison would use it. The caller closes the
// intake instead (OperatorIdentityCheck).
func OperatorUsername(namespace string) (string, error) {
	name := strings.TrimSpace(os.Getenv(ServiceAccountEnv))
	switch {
	case name == "":
		return "", fmt.Errorf("%w: %s is not set (the chart sets it from spec.serviceAccountName with the downward API; "+
			"a hand-written deployment must too)", ErrOperatorIdentity, ServiceAccountEnv)
	case namespace == "":
		return "", fmt.Errorf("%w: the operator's namespace is unknown (set POD_NAMESPACE)", ErrOperatorIdentity)
	}
	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return "", fmt.Errorf("%w: %s=%q is not a ServiceAccount name: %s", ErrOperatorIdentity, ServiceAccountEnv, name, problems[0])
	}
	if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
		return "", fmt.Errorf("%w: the operator namespace %q is not a namespace name: %s", ErrOperatorIdentity, namespace, problems[0])
	}
	return "system:serviceaccount:" + namespace + ":" + name, nil
}

// OperatorIdentityCheck is the preflight check for the operator's identity: it
// fails with err, the error OperatorUsername returned, or passes when it is nil.
func OperatorIdentityCheck(err error) Check {
	return StaticCheck("operator-identity", ReasonOperatorIdentityUnknown, err)
}
