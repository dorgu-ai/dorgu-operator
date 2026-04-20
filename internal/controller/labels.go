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

const (
	LabelSource               = "dorgu.io/source"
	LabelSourceAutoDiscovered = "auto-discovered"
	LabelSourceUserDefined    = "user-defined"
	LabelWorkloadKind         = "dorgu.io/workload-kind"    // "Deployment" | "StatefulSet"
	LabelWorkloadName         = "dorgu.io/workload-name"    // workload metadata.name
	LabelWorkloadDeleted      = "dorgu.io/workload-deleted" // "true" when source workload deleted

	AnnotationDiscoveryTimestamp = "dorgu.io/discovery-timestamp"
	AnnotationWorkloadImage      = "dorgu.io/workload-image"
)
