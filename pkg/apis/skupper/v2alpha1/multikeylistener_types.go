/*
Copyright 2025.

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

package v2alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MultiKeyListenerSpec defines the desired state of MultiKeyListener
type MultiKeyListenerSpec struct {
	// host is the hostname or IP address of the local listener. Clients at
	// this site use the listener host and port to establish connections to the
	// remote service.
	Host string `json:"host"`

	// port of the local listener. Clients at this site use the listener host
	// and port to establish connections to the remote service.
	Port int `json:"port"`

	// tlsCredentials is the name of a bundle of TLS certificates used for
	// secure client-to-router communication. The bundle contains the server
	// certificate and key. It optionally includes the trusted client
	// certificate (usually a CA) for mutual TLS. On Kubernetes, the value is
	// the name of a Secret in the current namespace. On Docker, Podman, and
	// Linux, the value is the name of a directory under input/certs/ in the
	// current namespace.
	// +optional
	TlsCredentials string `json:"tlsCredentials,omitempty"`

	// strategy to use for routing local traffic across routing keys with
	// reachable destinations. Options: ["priority"]
	Strategy string `json:"strategy"`

	// options are defined per-strategy
	// +optional
	Options *runtime.RawExtension `json:"options,omitempty"`

	// routingKeys is the list of routingKeys over which the listener will
	// apply a strategy in order to route service traffic.
	RoutingKeys []RoutingKeyOptions `json:"routingKeys"`
}

type RoutingKeyOptions struct {
	// routingKey is the identifier to route traffic from listeners to
	// connectors.
	RoutingKey string `json:"routingKey"`
	// options for routingKeys are defined per-strategy
	// +optional
	Options *runtime.RawExtension `json:"options,omitempty"`
}

// MultiKeyListenerStatus defines the observed state of MultiKeyListener.
type MultiKeyListenerStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	StatusType string             `json:"status,omitempty"`
	Message    string             `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MultiKeyListener is the Schema for the multikeylisteners API
type MultiKeyListener struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of MultiKeyListener
	// +required
	Spec MultiKeyListenerSpec `json:"spec"`

	// status defines the observed state of MultiKeyListener
	// +optional
	Status MultiKeyListenerStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// MultiKeyListenerList contains a list of MultiKeyListener
type MultiKeyListenerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MultiKeyListener `json:"items"`
}
