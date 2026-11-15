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
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The operator may read exactly these two CRDs, by name, to check them.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=remediationactions.dorgu.io;remediationproposals.dorgu.io,verbs=get

// The CRDs the intake needs, by name, and the files that install them. Helm installs
// a chart's crds/ directory on install but never on upgrade, so on an upgrade both
// files are applied by hand before `helm upgrade`.
const (
	ProposalCRDName = "remediationproposals.dorgu.io"
	ActionCRDName   = "remediationactions.dorgu.io"

	proposalCRDFile = "dorgu.io_remediationproposals.yaml"
	actionCRDFile   = "dorgu.io_remediationactions.yaml"

	proposalVersion = "v1alpha1"
	actionVersion   = "v1"
)

// ProposalCRDCheck is the preflight check that the RemediationProposal CRD is
// installed, established and served. Without it the proposal controller's cache can
// never sync and the manager would exit, taking detection down with it; with this
// check the operator reports the problem and carries on.
//
// reader should be uncached (the manager's API reader).
func ProposalCRDCheck(reader client.Reader) Check {
	return Check{Name: "proposal-crd", Reason: ReasonProposalCRDMissing, Run: func(ctx context.Context) error {
		crd, err := readCRD(ctx, reader, ProposalCRDName, proposalCRDFile)
		if err != nil {
			return err
		}
		if _, ok := servedVersion(crd, proposalVersion); !ok {
			return fmt.Errorf("the installed %s does not serve %s: apply %s from this release", ProposalCRDName, proposalVersion, proposalCRDFile)
		}
		return nil
	}}
}

// ActionProvenanceCheck is the preflight check that the installed RemediationAction
// CRD has spec.provenance in its schema. An older CRD does not, and the API server
// then silently drops provenance from every action the operator creates: each reads
// as an action that predates the intake, which is to say as proposed by the operator
// itself. That is the fail-open direction for the approval rules, so the intake
// stays closed until the CRD is current.
func ActionProvenanceCheck(reader client.Reader) Check {
	return Check{Name: "action-provenance", Reason: ReasonActionProvenanceMissing, Run: func(ctx context.Context) error {
		crd, err := readCRD(ctx, reader, ActionCRDName, actionCRDFile)
		if err != nil {
			return err
		}
		version, ok := servedVersion(crd, actionVersion)
		if !ok {
			return fmt.Errorf("the installed %s does not serve %s: apply %s from this release", ActionCRDName, actionVersion, actionCRDFile)
		}
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			return missingProvenance()
		}
		spec, ok := version.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			return missingProvenance()
		}
		if _, ok := spec.Properties["provenance"]; !ok {
			return missingProvenance()
		}
		return nil
	}}
}

func missingProvenance() error {
	return fmt.Errorf("the installed %s has no spec.provenance in its schema, so the API server would drop the provenance "+
		"of every action the operator creates: apply %s from this release before upgrading the operator", ActionCRDName, actionCRDFile)
}

// readCRD reads a CRD and says what is wrong in words that name the file to apply.
func readCRD(ctx context.Context, reader client.Reader, name, file string) (*apiextensionsv1.CustomResourceDefinition, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, &crd); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the CRD %s is not installed: apply %s from this release, then the operator picks it up on its own", name, file)
		}
		return nil, fmt.Errorf("could not read the CRD %s (the operator needs get on that one CRD): %w", name, err)
	}
	if !established(&crd) {
		return nil, fmt.Errorf("the CRD %s is not established yet (if it stays that way, re-apply %s from this release)", name, file)
	}
	return &crd, nil
}

func established(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Established {
			return c.Status == apiextensionsv1.ConditionTrue
		}
	}
	return false
}

func servedVersion(crd *apiextensionsv1.CustomResourceDefinition, name string) (apiextensionsv1.CustomResourceDefinitionVersion, bool) {
	for _, v := range crd.Spec.Versions {
		if v.Name == name && v.Served {
			return v, true
		}
	}
	return apiextensionsv1.CustomResourceDefinitionVersion{}, false
}
