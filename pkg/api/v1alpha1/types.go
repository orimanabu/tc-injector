package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +groupName=tc-injector.example.com

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
}

// DelayRule pairs a pod label selector with a delay range in milliseconds.
type DelayRule struct {
	// Selector selects pods to inject delay into.
	Selector metav1.LabelSelector `json:"selector"`
	// MinDelay is the minimum delay in milliseconds.
	// +kubebuilder:validation:Minimum=0
	MinDelay int32 `json:"minDelay"`
	// MaxDelay is the maximum delay in milliseconds. Must be >= MinDelay.
	// +kubebuilder:validation:Minimum=0
	MaxDelay int32 `json:"maxDelay"`
}

// TCInjectorStatus reports current injection state.
type TCInjectorStatus struct {
	// InjectedPods is the number of pods currently receiving delay injection on this node.
	InjectedPods int32 `json:"injectedPods,omitempty"`
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
