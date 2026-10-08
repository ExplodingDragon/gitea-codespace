// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"gitea.dev/codespace/internal/devcontainerruntime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var GroupVersion = schema.GroupVersion{Group: "codespace.gitea.dev", Version: "v1alpha1"}

func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &GiteaSite{}, &GiteaSiteList{}, &EnvironmentTemplate{}, &EnvironmentTemplateList{}, &Codespace{}, &CodespaceList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

// ResourceReference pins an authorization to an object, rather than a reusable name.
type ResourceReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Type=string
	UID types.UID `json:"uid"`
}

// ObservedResource also records a reserved name before the API assigns its UID.
type ObservedResource struct {
	Name string    `json:"name,omitempty"`
	UID  types.UID `json:"uid,omitempty"`
}

type GiteaSiteSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	DisplayName string `json:"displayName"`
	URL         string `json:"url"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Gitea Manager identity is immutable"
	ManagerID     int64             `json:"managerID"`
	Credential    ResourceReference `json:"credential"`
	Enabled       bool              `json:"enabled"`
	AcceptCreates bool              `json:"acceptCreates"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	StartupConcurrency int32 `json:"startupConcurrency"`
	// +kubebuilder:default=16
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	CleanupConcurrency int32 `json:"cleanupConcurrency"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Templates       []ResourceReference   `json:"templates"`
	Gateway         ResourceReference     `json:"gateway"`
	Caches          []ResourceReference   `json:"caches,omitempty"`
	Quota           corev1.ResourceList   `json:"quota"`
	ContainerLimits corev1.LimitRangeItem `json:"containerLimits"`
	// Upstreams are explicit exceptions to the workload's private-network isolation.
	Upstreams []NetworkDestination `json:"upstreams,omitempty"`
}

type NetworkDestination struct {
	CIDR  string  `json:"cidr"`
	Ports []int32 `json:"ports"`
}

type GiteaSiteStatus struct {
	NamespaceUID        types.UID `json:"namespaceUID,omitempty"`
	InventoryGeneration int64     `json:"inventoryGeneration,omitempty"`
	CanonicalURL        string    `json:"canonicalURL,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gsite
// +kubebuilder:subresource:status
type GiteaSite struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GiteaSiteSpec   `json:"spec"`
	Status            GiteaSiteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GiteaSiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GiteaSite `json:"items"`
}

type EnvironmentTemplateSpec struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9_-]{1,64}$`
	Tag string `json:"tag"`
	// +kubebuilder:validation:MaxLength=255
	Description string                   `json:"description,omitempty"`
	Runtime     EnvironmentConfiguration `json:"runtime"`
}

// EnvironmentConfiguration defines administrator-selected runtime behavior.
type EnvironmentConfiguration struct {
	// +kubebuilder:validation:Enum=kata;sysbox
	Isolation        string                      `json:"isolation"`
	RuntimeClassName string                      `json:"runtimeClassName"`
	StorageClassName string                      `json:"storageClassName"`
	Storage          corev1.ResourceList         `json:"storage"`
	Resources        corev1.ResourceRequirements `json:"resources"`
	// VolumeMode controls whether Kubernetes mounts the volume on the host or
	// exposes a raw block device for the isolated runtime to mount itself.
	// +kubebuilder:validation:Enum=Filesystem;Block
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode"`
	// +kubebuilder:validation:Enum=ReadWriteOncePod;ReadWriteOnce
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode"`
	// +kubebuilder:validation:Enum=ed25519;rsa-4096
	GitSSHKeyType string                            `json:"gitSSHKeyType"`
	DevContainer  devcontainerruntime.Configuration `json:"devContainer"`
}

// RuntimeConfiguration is fixed at creation so template and release edits do
// not change recovery behavior for an existing Codespace.
type RuntimeConfiguration struct {
	EnvironmentConfiguration `json:",inline"`
	Image                    string `json:"image"`
}

type EnvironmentTemplateStatus struct {
	// Verification is an administrator's reference to a real runtime/storage test record.
	VerifiedGeneration int64  `json:"verifiedGeneration,omitempty"`
	Verification       string `json:"verification,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=csenv
// +kubebuilder:subresource:status
type EnvironmentTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EnvironmentTemplateSpec   `json:"spec"`
	Status            EnvironmentTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type EnvironmentTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EnvironmentTemplate `json:"items"`
}

type Operation struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="operation version must not decrease"
	Version int64 `json:"version"`
	// +kubebuilder:validation:Enum=create;resume;stop;delete;abort_create;abort_resume
	Type string `json:"type"`
	// Payload contains the typed Gitea operation in its canonical protobuf JSON form.
	// +kubebuilder:pruning:PreserveUnknownFields
	Payload runtime.RawExtension `json:"payload"`
}

type CodespaceSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="site identity is immutable"
	Site ResourceReference `json:"site"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Gitea identity is immutable"
	CodespaceID int64 `json:"codespaceID"`
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime identity is immutable"
	RuntimeUUID string `json:"runtimeUUID"`
	// +kubebuilder:validation:Pattern=`^[a-z0-9_-]{1,64}$`
	EnvironmentTag string `json:"environmentTag"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime configuration is immutable"
	Runtime   RuntimeConfiguration `json:"runtime"`
	Operation Operation            `json:"operation"`
}

type AgentTarget struct {
	Version            int64           `json:"version"`
	Ready              bool            `json:"ready"`
	PrimaryContainerID string          `json:"primaryContainerID"`
	Endpoints          []AgentEndpoint `json:"endpoints"`
}

type AgentEndpoint struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Public      bool   `json:"public"`
	ContainerID string `json:"containerID"`
	Port        int32  `json:"port"`
}

// RuntimeBoot is the durable, monotonic startup progress reported by Agent.
type RuntimeBoot struct {
	OperationVersion int64  `json:"operationVersion"`
	Stage            string `json:"stage"`
	StartedUnix      int64  `json:"startedUnix"`
	LastUpdateUnix   int64  `json:"lastUpdateUnix"`
}

type OperationResult struct {
	Version   int64  `json:"version"`
	Succeeded bool   `json:"succeeded"`
	Message   string `json:"message,omitempty"`
}

type CodespaceStatus struct {
	Bound        bool             `json:"bound,omitempty"`
	Pod          ObservedResource `json:"pod,omitempty"`
	LastNodeName string           `json:"lastNodeName,omitempty"`
	// StoppedPodUID is recorded from kubelet termination before releasing the Pod finalizer.
	StoppedPodUID      types.UID        `json:"stoppedPodUID,omitempty"`
	Volume             ObservedResource `json:"volume,omitempty"`
	IdentitySecretName string           `json:"identitySecretName,omitempty"`
	MetadataGeneration int64            `json:"metadataGeneration,omitempty"`
	// SettledOperationVersion records Gitea's terminal operation acknowledgement.
	SettledOperationVersion int64 `json:"settledOperationVersion,omitempty"`
	// RecoveryAction is an inventory-authorized resource action, not a new Gitea operation.
	// +kubebuilder:validation:Enum=stop;delete
	RecoveryAction string           `json:"recoveryAction,omitempty"`
	Boot           *RuntimeBoot     `json:"boot,omitempty"`
	Target         *AgentTarget     `json:"target,omitempty"`
	Result         *OperationResult `json:"result,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=cs
// +kubebuilder:subresource:status
type Codespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CodespaceSpec   `json:"spec"`
	Status            CodespaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CodespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Codespace `json:"items"`
}
