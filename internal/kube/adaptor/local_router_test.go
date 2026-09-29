package adaptor

import "testing"

func TestManagementRecoveryInvalidatesRouterRealization(t *testing.T) {
	router := &AMQPLocalRouter{}
	first := router.managementSucceeded()
	if got := router.MarkRouterVerified(); got != first {
		t.Fatalf("verified incarnation %q, want %q", got, first)
	}
	if _, verified := router.CurrentRouterIncarnation(); !verified {
		t.Fatal("fresh realization was not verified")
	}
	router.managementFailed()
	if got, verified := router.CurrentRouterIncarnation(); got != first || verified {
		t.Fatalf("management failure left stale evidence current: %q verified=%v", got, verified)
	}
	second := router.managementSucceeded()
	if second == first {
		t.Fatal("management recovery reused stale router incarnation")
	}
	if _, verified := router.CurrentRouterIncarnation(); verified {
		t.Fatal("reconnected management was trusted before fresh realization")
	}
}
