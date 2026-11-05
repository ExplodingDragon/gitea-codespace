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

const (
	IsolationKata         = "kata"
	IsolationSysbox       = "sysbox"
	RuntimeIsolationLabel = "codespace.gitea.dev/isolation"
)

func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &GiteaSite{}, &GiteaSiteList{}, &EnvironmentTemplate{}, &EnvironmentTemplateList{}, &CodespaceComponent{}, &CodespaceComponentList{}, &Codespace{}, &CodespaceList{})
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
	// CABundle extends the system trust store for this Gitea site.
	CABundle string `json:"caBundle,omitempty"`
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
	Templates []ResourceReference `json:"templates"`
	Gateway   ResourceReference   `json:"gateway"`
	Caches    []ResourceReference `json:"caches,omitempty"`
	// Quota is empty when Kubernetes admission alone manages site capacity.
	Quota corev1.ResourceList `json:"quota,omitempty"`
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

// OutboundProxy configures one administrator-managed network proxy. Credentials
// are stored separately because Kubernetes API objects are not secret storage.
type OutboundProxy struct {
	// +kubebuilder:validation:MaxLength=2048
	HTTPProxy string `json:"httpProxy,omitempty"`
	// +kubebuilder:validation:MaxLength=2048
	HTTPSProxy string `json:"httpsProxy,omitempty"`
	// +kubebuilder:validation:MaxLength=2048
	SOCKSProxy string `json:"socksProxy,omitempty"`
	// +kubebuilder:validation:MaxItems=128
	NoProxy []string `json:"noProxy,omitempty"`
	// CABundle extends the system trust store for the configured proxy.
	// +kubebuilder:validation:MaxLength=262144
	CABundle string `json:"caBundle,omitempty"`
}

// EnvironmentConfiguration defines administrator-selected runtime behavior.
type EnvironmentConfiguration struct {
	// +kubebuilder:validation:Enum=kata;sysbox
	Isolation        string                      `json:"isolation"`
	RuntimeClassName string                      `json:"runtimeClassName"`
	StorageClassName string                      `json:"storageClassName"`
	Storage          corev1.ResourceList         `json:"storage"`
	Resources        corev1.ResourceRequirements `json:"resources"`
	// +kubebuilder:validation:Enum=ReadWriteOncePod;ReadWriteOnce
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode"`
	// +kubebuilder:validation:Enum=ed25519;rsa-4096
	GitSSHKeyType string                            `json:"gitSSHKeyType"`
	DevContainer  devcontainerruntime.Configuration `json:"devContainer"`
	Proxy         *OutboundProxy                    `json:"proxy,omitempty"`
}

// StorageVolumeMode returns the volume representation required by the selected
// isolation runtime. Kata mounts Docker data inside the guest, while Sysbox
// receives the filesystem mount directly from Kubernetes.
func (r EnvironmentConfiguration) StorageVolumeMode() corev1.PersistentVolumeMode {
	if r.Isolation == IsolationKata {
		return corev1.PersistentVolumeBlock
	}
	return corev1.PersistentVolumeFilesystem
}

// RuntimeConfiguration is fixed at creation so template and release edits do
// not change recovery behavior for an existing Codespace.
type RuntimeConfiguration struct {
	EnvironmentConfiguration `json:",inline"`
	Image                    string `json:"image"`
}

type EnvironmentTemplateStatus struct {
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

type GatewayIssuerReference struct {
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type GatewayIngressTLS struct {
	// +kubebuilder:validation:Enum=none;secret;certManager
	Source     string                  `json:"source"`
	SecretName string                  `json:"secretName,omitempty"`
	Issuer     *GatewayIssuerReference `json:"issuer,omitempty"`
}

type GatewayIngressExposure struct {
	ClassName string            `json:"className"`
	TLS       GatewayIngressTLS `json:"tls"`
}

type GatewayHTTPExposure struct {
	// +kubebuilder:validation:Enum=external;ingress
	Mode    string                  `json:"mode"`
	Ingress *GatewayIngressExposure `json:"ingress,omitempty"`
}

type GatewaySSHExposure struct {
	// +kubebuilder:validation:Enum=external;loadBalancer
	Mode string `json:"mode"`
}

type GatewaySSHAuth struct {
	// +kubebuilder:validation:Minimum=1
	MaxAttemptsPerIP int32 `json:"maxAttemptsPerIP"`
	// +kubebuilder:validation:Minimum=1
	MaxAttemptsPerCodespace int32 `json:"maxAttemptsPerCodespace"`
	// +kubebuilder:validation:Minimum=1
	MaxAttemptsPerIPCodespace int32 `json:"maxAttemptsPerIPCodespace"`
	// +kubebuilder:validation:Minimum=1
	MaxAttemptsPerPublicKey int32 `json:"maxAttemptsPerPublicKey"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	FailureWindow string `json:"failureWindow"`
}

type GatewaySSHSettings struct {
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	HandshakeTimeout string `json:"handshakeTimeout"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxChannelsPerConnection int32          `json:"maxChannelsPerConnection"`
	Auth                     GatewaySSHAuth `json:"auth"`
}

type GatewaySessions struct {
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	TTL string `json:"ttl"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	IdleTimeout string `json:"idleTimeout"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	RevalidateInterval string `json:"revalidateInterval"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxPerCodespace int32 `json:"maxPerCodespace"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxPerUser int32 `json:"maxPerUser"`
}

type GatewayLimits struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxInflightTotal int32 `json:"maxInflightTotal"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxInflightPerSession int32 `json:"maxInflightPerSession"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	PublicMaxConnectionsPerEndpoint int32 `json:"publicMaxConnectionsPerEndpoint"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	PublicMaxConnectionsPerIP int32 `json:"publicMaxConnectionsPerIP"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4096
	ValidationMaxInflight int32 `json:"validationMaxInflight"`
}

type GatewayComponentSpec struct {
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Replicas           int32               `json:"replicas"`
	EndpointBaseURL    string              `json:"endpointBaseURL"`
	SSHExternalAddress string              `json:"sshExternalAddress"`
	HTTPExposure       GatewayHTTPExposure `json:"httpExposure"`
	SSHExposure        GatewaySSHExposure  `json:"sshExposure"`
	// +kubebuilder:validation:MaxItems=64
	TrustedProxyCIDRs []string           `json:"trustedProxyCIDRs,omitempty"`
	SSH               GatewaySSHSettings `json:"ssh"`
	Sessions          GatewaySessions    `json:"sessions"`
	Limits            GatewayLimits      `json:"limits"`
}

type CacheS3Storage struct {
	Endpoint       string `json:"endpoint,omitempty"`
	Region         string `json:"region,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	ForcePathStyle bool   `json:"forcePathStyle,omitempty"`
}

type CacheStorage struct {
	// +kubebuilder:validation:Enum=filesystem;s3
	Driver string `json:"driver"`
	// VolumeSize is the persistent volume capacity. Cache eviction is governed
	// independently by maxSize so operators can reserve free filesystem space.
	VolumeSize       string         `json:"volumeSize,omitempty"`
	StorageClassName string         `json:"storageClassName,omitempty"`
	MinFreeSpace     string         `json:"minFreeSpace,omitempty"`
	S3               CacheS3Storage `json:"s3,omitempty"`
}

type CacheUpstream struct {
	// +kubebuilder:validation:MinItems=1
	Allow []string `json:"allow"`
}

type CacheComponentSpec struct {
	Enabled bool         `json:"enabled"`
	Storage CacheStorage `json:"storage"`
	MaxSize string       `json:"maxSize,omitempty"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	MaxAge string `json:"maxAge"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|ms|s|m|h)$`
	GCInterval string                   `json:"gcInterval"`
	Upstreams  map[string]CacheUpstream `json:"upstreams,omitempty"`
	Proxy      *OutboundProxy           `json:"proxy,omitempty"`
}

type CodespaceComponentSpec struct {
	// +kubebuilder:validation:Enum=gateway;cache
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="component role is immutable"
	Role string `json:"role"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	DisplayName string                      `json:"displayName"`
	Resources   corev1.ResourceRequirements `json:"resources"`
	Gateway     *GatewayComponentSpec       `json:"gateway,omitempty"`
	Cache       *CacheComponentSpec         `json:"cache,omitempty"`
}

type CodespaceComponentStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cscomp
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 50 && !self.metadata.name.contains('.')",message="component name must be a Kubernetes DNS label of at most 50 characters"
// +kubebuilder:validation:XValidation:rule="self.spec.role == 'gateway' ? has(self.spec.gateway) && !has(self.spec.cache) : has(self.spec.cache) && !has(self.spec.gateway)",message="component role must select exactly one configuration"
type CodespaceComponent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CodespaceComponentSpec   `json:"spec"`
	Status            CodespaceComponentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CodespaceComponentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CodespaceComponent `json:"items"`
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
	StoppedPodUID types.UID        `json:"stoppedPodUID,omitempty"`
	Volume        ObservedResource `json:"volume,omitempty"`
	// A started format is never retried when the filesystem is unrecognizable.
	// +kubebuilder:validation:Enum=started;ready
	VolumeInitialization string `json:"volumeInitialization,omitempty"`
	MetadataGeneration   int64  `json:"metadataGeneration,omitempty"`
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
