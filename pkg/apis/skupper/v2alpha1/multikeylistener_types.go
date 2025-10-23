package v2alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name=Status,JSONPath=.status.status,description="The status of the multikeylistener",type=string
// +kubebuilder:printcolumn:name=Message,JSONPath=.status.message,description="Any human reandable message relevant to the multikeylistener",type=string
// +kubebuilder:printcolumn:name=HasDestination,JSONPath=.status.hasDestination,description="Whether there is at least one connector in the network matched by the strategy",type=boolean
//

// MultiKeyListeners bind a local connection endpoint to Connectors across the
// Skupper network. A MultiKeyListener has a strategy that matches it to
// Connector routing keys.
type MultiKeyListener struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata"`
	// +required
	Spec MultiKeyListenerSpec `json:"spec"`
	// +optional
	Status MultiKeyListenerStatus `json:"status"`
}

// +kubebuilder:object:root=true

// MultiKeyListenerList contains a list of MultiKeyListener
type MultiKeyListenerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MultiKeyListener `json:"items"`
}

type MultiKeyListenerStatus struct {
	// conditions describing the current state of the multikeylistener
	//
	// - `Configured`: The multikeylistener configuration has been applied to the router.
	// - `Operational`: There is at least one connector corresponding to the multikeylistener strategy.
	// - `Ready`: The multikeylistener is ready to use. All other conditions are true..
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// The current state of the resource.
	// - `Pending`: The resource is being processed.
	// - `Error`: There was an error processing the resource. See `message` for more information.
	// - `Ready`: The resource is ready to use.
	StatusType StatusType `json:"status,omitempty"`
	// A human-readable status message. Error messages are reported here.
	Message string `json:"message,omitempty"`
	// hasDestination is set true when there is at least one connector in the
	// network with a routing key matched by the strategy.
	HasDestination bool `json:"hasDestination,omitempty"`

	Strategy *StrategyStatus `json:"strategy,omitempty"`
}

// +kubebuilder:validation:ExactlyOneOf=priorityFailover
type StrategyStatus struct {
	// priorityFailover status
	PriorityFailover *PriorityFailoverStrategyStatus `json:"priorityFailover,omitempty"`
}

type PriorityFailoverStrategyStatus struct {
	// routingKeysReachable is a list of routingKeys with at least one
	// reachable connector given in priority order.
	RoutingKeysReachable []string `json:"routingKeysReachable"`
}

type MultiKeyListenerSpec struct {
	// host is the hostname or IP address of the local listener. Clients at
	// this site use the listener host and port to establish connections to the
	// remote service.
	Host string `json:"host"`
	// port of the local listener. Clients at this site use the listener host
	// and port to establish connections to the remote service.
	Port int `json:"port"`
	// tlsCredentials for client-to-listener
	TlsCredentials string `json:"tlsCredentials,omitempty"`
	// requireClientCert indicates that clients must present valid certificates
	// to the listener to connect.
	RequireClientCert bool `json:"requireClientCert,omitempty"`

	// settings is a map containing additional settings.
	//
	// **Note:** In general, we recommend not changing settings from
	// their default values.
	Settings map[string]string `json:"settings,omitempty"`

	// strategy for routing traffic from the local listener endpoint to one or
	// more connector instances by routing key.
	Strategy MultiKeyListenerStrategy `json:"strategy"`
}

// MultiKeyListenerStrategy contains configuration for each strategy type.
//
// +kubebuilder:validation:ExactlyOneOf=priorityFailover
// +kubebuilder:validation:XValidation:rule="(has(self.priorityFailover) ? self.type == 'PriorityFailover' : true)",message="field priorityFailover is not allowed for types other than `PriorityFailover`"
type MultiKeyListenerStrategy struct {
	// +kubebuilder:validation:Enum=PriorityFailover;RoundRobin;
	//
	// type of the strategy. Must be one of the following:
	//
	// - PriorityFailover
	Type string `json:"type"`
	// priorityFailover configuration. Valid only when type == PriorityFailover
	PriorityFailover *PriorityFailoverStrategySpec `json:"priorityFailover,omitempty"`
}

type PriorityFailoverStrategySpec struct {
	// +kubebuilder:validation:MinItems=1
	// +listType=set

	// routingKeys to route traffic to in priority order.
	//
	// With this strategy 100% of traffic will be directed to the first
	// routingKey with a reachable connector.
	RoutingKeys []string `json:"routingKeys"`
}
