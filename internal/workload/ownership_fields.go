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

package workload

import (
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file reads `managedFields[].fieldsV1` to answer one question: does this
// field manager own the fields a Dorgu remediation would write?
//
// fieldsV1 is a set encoded as nested JSON objects, one key per path segment:
//
//	{"f:spec":{"f:template":{"f:spec":{"f:containers":{
//	   "k:{\"name\":\"report-worker\"}":{"f:resources":{"f:limits":{"f:memory":{}}}}}}}}}
//
// Prefixes: `f:` is a field name, `k:` is an associative-list key (containers
// are keyed by name), `v:` a set member and `i:` a list index. Only `f:` and
// `k:` occur on the path this cares about.

// fieldsV1 path segments down to a container's resource block.
const (
	fieldSpec       = "f:spec"
	fieldTemplate   = "f:template"
	fieldContainers = "f:containers"
	fieldResources  = "f:resources"

	// containerKeyPrefix marks an associative-list entry. Containers are keyed
	// by name, so every child of `f:containers` is a `k:{"name":"..."}` key.
	containerKeyPrefix = "k:"
)

// ownsContainerResources reports whether a managedFields entry claims any
// container's `resources` block, which is the only part of a Deployment a
// Dorgu remediation writes.
//
// The check is scoped to those fields on purpose. A manager that owns only
// `spec.replicas` (an autoscaler) or a pod-template annotation (a sidecar
// injector) is not in the way of a resource patch, and treating it as an owner
// would make Dorgu refuse to heal on most real clusters for no safety gain.
//
// Any container counts, not just the one a fix would target. A manager holding
// the resources of a sibling container is reconciling this pod template, and
// which container Dorgu ends up patching is decided later than this.
//
// An entry whose fieldsV1 cannot be parsed is reported as owning. The house
// rule is that absence of evidence that patching is safe is not evidence that
// it is, and a managedFields entry Dorgu cannot read is exactly that.
func ownsContainerResources(entry metav1.ManagedFieldsEntry) bool {
	if entry.FieldsV1 == nil || len(entry.FieldsV1.Raw) == 0 {
		return false
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(entry.FieldsV1.Raw, &root); err != nil {
		return true
	}

	containers, ok := descendFields(root, fieldSpec, fieldTemplate, fieldSpec, fieldContainers)
	if !ok {
		return false
	}

	for key, raw := range containers {
		if !strings.HasPrefix(key, containerKeyPrefix) {
			continue
		}
		var container map[string]json.RawMessage
		if err := json.Unmarshal(raw, &container); err != nil {
			return true
		}
		if _, claimed := container[fieldResources]; claimed {
			return true
		}
	}
	return false
}

// descendFields walks a fieldsV1 tree along the given keys, returning the node
// it lands on. It reports false as soon as a key is missing or a node is not an
// object, which is the "this manager holds nothing on that path" answer.
func descendFields(node map[string]json.RawMessage, keys ...string) (map[string]json.RawMessage, bool) {
	for _, key := range keys {
		raw, ok := node[key]
		if !ok {
			return nil, false
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, false
		}
		node = next
	}
	return node, true
}
