package networkobserver

import (
	"slices"
	"testing"
)

func TestNetworkObserverNetworkID(t *testing.T) {
	for _, namespace := range []string{"default", "network-a", "network-b"} {
		c := GetNetworkObserverContainer(namespace, ports{})
		if !slices.Contains(c.Command, "-network-id="+namespace) {
			t.Errorf("namespace %q missing from observer arguments: %v", namespace, c.Command)
		}
	}
}
