package client

import (
	"testing"

	routefake "github.com/openshift/client-go/route/clientset/versioned/fake"
)

func TestRouteClientIsOptional(t *testing.T) {
	client := &KubeClient{}
	if client.GetRouteClient() != nil {
		t.Fatal("Kubernetes without the Route API must not expose a Route client")
	}
	client.Route = routefake.NewSimpleClientset()
	if client.GetRouteClient() == nil {
		t.Fatal("an available Route API lost its client")
	}
}
