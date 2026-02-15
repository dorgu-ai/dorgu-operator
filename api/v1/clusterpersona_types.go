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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// Spec types (owned by admin / GitOps)
// ============================================================================

// ClusterPersonaSpec defines the desired state of a cluster's identity.
type ClusterPersonaSpec struct {
	// name is the cluster's friendly name.
	// +required
	Name string `json:"name"`

	// description provides context about this cluster.
	// +optional
	Description string `json:"description,omitempty"`

	// environment indicates the cluster's purpose.
	// +kubebuilder:validation:Enum=development;staging;production;sandbox
	// +kubebuilder:default=development
	// +optional
	Environment string `json:"environment,omitempty"`

	// policies defines cluster-wide security and governance rules.
	// +optional
	Policies *ClusterPolicies `json:"policies,omitempty"`

	// conventions defines naming and labeling standards.
	// +optional
	Conventions *ClusterConventions `json:"conventions,omitempty"`

	// defaults defines default values for applications in this cluster.
	// +optional
	Defaults *ClusterDefaults `json:"defaults,omitempty"`

	// resourceQuotas defines cluster-wide resource constraints.
	// +optional
	ResourceQuotas *ClusterResourceQuotas `json:"resourceQuotas,omitempty"`
}

// ClusterPolicies defines cluster-wide security and governance policies.
type ClusterPolicies struct {
	// security defines security constraints for all workloads.
	// +optional
	Security *ClusterSecurityPolicy `json:"security,omitempty"`

	// networking defines network policies.
	// +optional
	Networking *ClusterNetworkPolicy `json:"networking,omitempty"`

	// compliance lists compliance frameworks this cluster adheres to.
	// +optional
	Compliance []string `json:"compliance,omitempty"`
}

// ClusterSecurityPolicy defines cluster-wide security constraints.
type ClusterSecurityPolicy struct {
	// enforceNonRoot requires all containers to run as non-root.
	// +kubebuilder:default=true
	// +optional
	EnforceNonRoot *bool `json:"enforceNonRoot,omitempty"`

	// enforceReadOnlyRoot requires read-only root filesystems.
	// +kubebuilder:default=false
	// +optional
	EnforceReadOnlyRoot *bool `json:"enforceReadOnlyRoot,omitempty"`

	// disallowPrivileged prevents privileged containers.
	// +kubebuilder:default=true
	// +optional
	DisallowPrivileged *bool `json:"disallowPrivileged,omitempty"`

	// allowedRegistries limits which container registries can be used.
	// +optional
	AllowedRegistries []string `json:"allowedRegistries,omitempty"`

	// podSecurityStandard sets the Pod Security Standard level.
	// +kubebuilder:validation:Enum=privileged;baseline;restricted
	// +kubebuilder:default=baseline
	// +optional
	PodSecurityStandard string `json:"podSecurityStandard,omitempty"`
}

// ClusterNetworkPolicy defines cluster-wide network constraints.
type ClusterNetworkPolicy struct {
	// defaultDenyIngress enables default deny for ingress traffic.
	// +kubebuilder:default=false
	// +optional
	DefaultDenyIngress *bool `json:"defaultDenyIngress,omitempty"`

	// defaultDenyEgress enables default deny for egress traffic.
	// +kubebuilder:default=false
	// +optional
	DefaultDenyEgress *bool `json:"defaultDenyEgress,omitempty"`

	// allowedExternalCIDRs lists allowed external IP ranges.
	// +optional
	AllowedExternalCIDRs []string `json:"allowedExternalCIDRs,omitempty"`
}

// ClusterConventions defines naming and labeling standards.
type ClusterConventions struct {
	// namingPattern defines the pattern for resource names.
	// +optional
	NamingPattern string `json:"namingPattern,omitempty"`

	// requiredLabels lists labels that must be present on all resources.
	// +optional
	RequiredLabels []string `json:"requiredLabels,omitempty"`

	// requiredAnnotations lists annotations that must be present.
	// +optional
	RequiredAnnotations []string `json:"requiredAnnotations,omitempty"`

	// namespacePrefix defines the prefix for namespace names.
	// +optional
	NamespacePrefix string `json:"namespacePrefix,omitempty"`
}

// ClusterDefaults defines default values for applications.
type ClusterDefaults struct {
	// namespace is the default namespace for new applications.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// registry is the default container registry.
	// +optional
	Registry string `json:"registry,omitempty"`

	// resources defines default resource requests/limits.
	// +optional
	Resources *ResourceConstraints `json:"resources,omitempty"`

	// scaling defines default scaling parameters.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`
}

// ClusterResourceQuotas defines cluster-wide resource constraints.
type ClusterResourceQuotas struct {
	// maxPodsPerNamespace limits pods per namespace.
	// +optional
	MaxPodsPerNamespace *int32 `json:"maxPodsPerNamespace,omitempty"`

	// maxCPUPerNamespace limits total CPU per namespace.
	// +optional
	MaxCPUPerNamespace string `json:"maxCPUPerNamespace,omitempty"`

	// maxMemoryPerNamespace limits total memory per namespace.
	// +optional
	MaxMemoryPerNamespace string `json:"maxMemoryPerNamespace,omitempty"`

	// maxPVCsPerNamespace limits PVCs per namespace.
	// +optional
	MaxPVCsPerNamespace *int32 `json:"maxPVCsPerNamespace,omitempty"`
}

// ============================================================================
// Status types (owned by operator)
// ============================================================================

// ClusterPersonaStatus defines the observed state of ClusterPersona.
type ClusterPersonaStatus struct {
	// phase summarizes the overall cluster state.
	// +kubebuilder:validation:Enum=Discovering;Ready;Degraded;Unknown
	// +optional
	Phase string `json:"phase,omitempty"`

	// lastDiscovery records when the operator last discovered cluster state.
	// +optional
	LastDiscovery *metav1.Time `json:"lastDiscovery,omitempty"`

	// nodes contains information about cluster nodes.
	// +optional
	Nodes []NodeInfo `json:"nodes,omitempty"`

	// resourceSummary contains cluster-wide resource usage.
	// +optional
	ResourceSummary *ClusterResourceSummary `json:"resourceSummary,omitempty"`

	// addons lists discovered cluster add-ons.
	// +optional
	Addons []AddonInfo `json:"addons,omitempty"`

	// namespaces contains namespace information.
	// +optional
	Namespaces *NamespaceSummary `json:"namespaces,omitempty"`

	// kubernetesVersion is the cluster's Kubernetes version.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// platform identifies the cluster platform (EKS, GKE, AKS, etc.).
	// +optional
	Platform string `json:"platform,omitempty"`

	// applicationCount is the number of ApplicationPersonas in the cluster.
	// +optional
	ApplicationCount int32 `json:"applicationCount,omitempty"`

	// conditions follow the standard Kubernetes condition pattern.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodeInfo contains information about a cluster node.
type NodeInfo struct {
	// name is the node name.
	Name string `json:"name"`

	// role indicates the node role (control-plane, worker).
	// +optional
	Role string `json:"role,omitempty"`

	// ready indicates if the node is ready.
	Ready bool `json:"ready"`

	// capacity contains the node's resource capacity.
	// +optional
	Capacity *NodeResources `json:"capacity,omitempty"`

	// allocatable contains the node's allocatable resources.
	// +optional
	Allocatable *NodeResources `json:"allocatable,omitempty"`

	// taints lists the node's taints.
	// +optional
	Taints []string `json:"taints,omitempty"`

	// labels contains key node labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// kubeletVersion is the node's kubelet version.
	// +optional
	KubeletVersion string `json:"kubeletVersion,omitempty"`

	// containerRuntime is the node's container runtime.
	// +optional
	ContainerRuntime string `json:"containerRuntime,omitempty"`
}

// NodeResources contains resource quantities for a node.
type NodeResources struct {
	// cpu is the CPU capacity/allocatable.
	// +optional
	CPU string `json:"cpu,omitempty"`

	// memory is the memory capacity/allocatable.
	// +optional
	Memory string `json:"memory,omitempty"`

	// pods is the pod capacity/allocatable.
	// +optional
	Pods string `json:"pods,omitempty"`

	// ephemeralStorage is the ephemeral storage capacity.
	// +optional
	EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}

// ClusterResourceSummary contains cluster-wide resource information.
type ClusterResourceSummary struct {
	// totalCPU is the total CPU capacity across all nodes.
	// +optional
	TotalCPU string `json:"totalCPU,omitempty"`

	// totalMemory is the total memory capacity across all nodes.
	// +optional
	TotalMemory string `json:"totalMemory,omitempty"`

	// allocatableCPU is the total allocatable CPU.
	// +optional
	AllocatableCPU string `json:"allocatableCPU,omitempty"`

	// allocatableMemory is the total allocatable memory.
	// +optional
	AllocatableMemory string `json:"allocatableMemory,omitempty"`

	// usedCPU is the currently used CPU (from metrics).
	// +optional
	UsedCPU string `json:"usedCPU,omitempty"`

	// usedMemory is the currently used memory (from metrics).
	// +optional
	UsedMemory string `json:"usedMemory,omitempty"`

	// cpuUtilization is the CPU utilization percentage.
	// +optional
	CPUUtilization string `json:"cpuUtilization,omitempty"`

	// memoryUtilization is the memory utilization percentage.
	// +optional
	MemoryUtilization string `json:"memoryUtilization,omitempty"`

	// totalPods is the total pod capacity.
	// +optional
	TotalPods int32 `json:"totalPods,omitempty"`

	// runningPods is the number of running pods.
	// +optional
	RunningPods int32 `json:"runningPods,omitempty"`
}

// AddonInfo contains information about a cluster add-on.
type AddonInfo struct {
	// name is the add-on name.
	Name string `json:"name"`

	// type categorizes the add-on.
	// +kubebuilder:validation:Enum=gitops;monitoring;logging;ingress;service-mesh;secrets;cert-management;other
	// +optional
	Type string `json:"type,omitempty"`

	// installed indicates if the add-on is detected.
	Installed bool `json:"installed"`

	// version is the detected version.
	// +optional
	Version string `json:"version,omitempty"`

	// namespace is where the add-on is installed.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// healthy indicates if the add-on is healthy.
	// +optional
	Healthy *bool `json:"healthy,omitempty"`
}

// NamespaceSummary contains namespace information.
type NamespaceSummary struct {
	// total is the total number of namespaces.
	Total int32 `json:"total"`

	// active is the number of active namespaces.
	// +optional
	Active int32 `json:"active,omitempty"`

	// withPersonas is the number of namespaces with ApplicationPersonas.
	// +optional
	WithPersonas int32 `json:"withPersonas,omitempty"`
}

// ============================================================================
// Root types
// ============================================================================

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.resourceSummary.totalPods`
// +kubebuilder:printcolumn:name="Apps",type=integer,JSONPath=`.status.applicationCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterPersona is the Schema for the clusterpersonas API.
// It represents the identity and configuration of a Kubernetes cluster.
// ClusterPersona is cluster-scoped (singleton per cluster).
type ClusterPersona struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterPersona.
	// +required
	Spec ClusterPersonaSpec `json:"spec"`

	// status defines the observed state of ClusterPersona.
	// +optional
	Status ClusterPersonaStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterPersonaList contains a list of ClusterPersona.
type ClusterPersonaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterPersona `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterPersona{}, &ClusterPersonaList{})
}
