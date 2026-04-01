package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +groupName=tc-injector.setns.net

// TCInjector defines network delay injection rules applied to matching pods.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type TCInjector struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TCInjectorSpec   `json:"spec,omitempty"`
	Status TCInjectorStatus `json:"status,omitempty"`
}

// TCInjectorSpec contains the list of delay injection rules.
type TCInjectorSpec struct {
	// Rules is a list of label selectors paired with delay parameters.
	Rules []DelayRule `json:"rules"`
	// EnablePeriodicDelayRotation enables periodic re-randomization of delays within [MinDelay, MaxDelay].
	// When false (default), delays are applied once and only updated on resource or pod changes.
	// +kubebuilder:default=false
	EnablePeriodicDelayRotation bool `json:"enablePeriodicDelayRotation,omitempty"`
	// DelayInterval is the interval between periodic delay re-randomizations.
	// Only takes effect when EnablePeriodicDelayRotation is true.
	// Defaults to 30s.
	// +kubebuilder:default="30s"
	DelayInterval *metav1.Duration `json:"delayInterval,omitempty"`
}

// DelayRule pairs a pod label selector and a namespace selector with a delay range in milliseconds.
type DelayRule struct {
	// Selector selects pods to inject delay into.
	Selector metav1.LabelSelector `json:"selector"`
	// NamespaceSelector selects namespaces whose pods are eligible for delay injection.
	// An empty selector matches all namespaces.
	// +optional
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	// MinDelay is the minimum delay in milliseconds.
	// +kubebuilder:validation:Minimum=0
	MinDelay int32 `json:"minDelay"`
	// MaxDelay is the maximum delay in milliseconds. Must be >= MinDelay.
	// +kubebuilder:validation:Minimum=0
	MaxDelay int32 `json:"maxDelay"`
	// MultusNetworks is an optional list of NetworkAttachmentDefinition names whose
	// pod-side interfaces should also receive delay injection.
	// Each entry may be "name" (matches any namespace) or "namespace/name" (exact match).
	// Interfaces are resolved via the k8s.v1.cni.cncf.io/networks-status pod annotation.
	// If empty, only the primary interface is targeted.
	// +optional
	MultusNetworks []string `json:"multusNetworks,omitempty"`
}

// InjectedInterfaceStatus describes a tc rule applied to a Multus-managed interface.
type InjectedInterfaceStatus struct {
	// NADName is the NetworkAttachmentDefinition identifier (namespace/name) as reported
	// by the Multus annotation.
	NADName string `json:"nadName"`
	// Interface is the name of the interface inside the pod (e.g. net1).
	Interface string `json:"interface"`
	// DelayMs is the injected delay in milliseconds.
	DelayMs int32 `json:"delayMs"`
	// TCCommand is the tc command line that was applied.
	TCCommand string `json:"tcCommand"`
}

// InjectedPodStatus describes the tc rule currently applied to a single pod.
type InjectedPodStatus struct {
	// NodeName is the node where this rule is applied.
	NodeName string `json:"nodeName"`
	// Namespace is the namespace of the target pod.
	Namespace string `json:"namespace"`
	// PodName is the name of the target pod.
	PodName string `json:"podName"`
	// Interface is the host-side network interface name.
	Interface string `json:"interface"`
	// InterfaceIndex is the ifindex of the host-side interface.
	InterfaceIndex int32 `json:"interfaceIndex"`
	// DelayMs is the injected delay in milliseconds.
	DelayMs int32 `json:"delayMs"`
	// TCCommand is the tc command line that was applied.
	TCCommand string `json:"tcCommand"`
	// MultusInterfaces lists tc rules applied to Multus-managed interfaces of this pod.
	// +optional
	MultusInterfaces []InjectedInterfaceStatus `json:"multusInterfaces,omitempty"`
}

// TCInjectorStatus reports current injection state.
type TCInjectorStatus struct {
	// InjectedPods is the number of pods currently receiving delay injection on this node.
	InjectedPods int32 `json:"injectedPods,omitempty"`
	// InjectedPodDetails lists the tc rule details for each injected pod on this node.
	InjectedPodDetails []InjectedPodStatus `json:"injectedPodDetails,omitempty"`
	// Conditions describes the current state of the TCInjector.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TCInjectorList contains a list of TCInjector.
// +kubebuilder:object:root=true
type TCInjectorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TCInjector `json:"items"`
}
