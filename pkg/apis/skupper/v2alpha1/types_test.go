package v2alpha1

import (
	"testing"

	"gotest.tools/v3/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConnectorConfiguredRetiresLegacyMatchingStatus(t *testing.T) {
	connector := &Connector{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: ConnectorStatus{
			HasMatchingListener: true,
			Status: Status{Conditions: []metav1.Condition{
				{Type: CONDITION_TYPE_CONFIGURED, Status: metav1.ConditionTrue, Reason: string(StatusReady), Message: STATUS_OK, ObservedGeneration: 3},
				{Type: CONDITION_TYPE_MATCHED, Status: metav1.ConditionFalse, Reason: string(StatusPending), Message: "No matching listeners"},
				{Type: CONDITION_TYPE_READY, Status: metav1.ConditionFalse, Reason: string(StatusPending), Message: "No matching listeners"},
			}},
		},
	}

	assert.Assert(t, connector.SetConfigured(nil))
	assert.Assert(t, !connector.Status.HasMatchingListener)
	assert.Assert(t, meta.FindStatusCondition(connector.Status.Conditions, CONDITION_TYPE_MATCHED) == nil)
	assert.Equal(t, meta.FindStatusCondition(connector.Status.Conditions, CONDITION_TYPE_READY).Status, metav1.ConditionTrue)
	assert.Assert(t, !connector.SetConfigured(nil), "unchanged configured status should not be rewritten")
}

func TestAttachedConnectorBindingConfiguredRetiresLegacyMatchingStatus(t *testing.T) {
	binding := &AttachedConnectorBinding{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: AttachedConnectorBindingStatus{
			HasMatchingListener: true,
			Status: Status{Conditions: []metav1.Condition{
				{Type: CONDITION_TYPE_CONFIGURED, Status: metav1.ConditionTrue, Reason: string(StatusReady), Message: STATUS_OK, ObservedGeneration: 3},
				{Type: CONDITION_TYPE_MATCHED, Status: metav1.ConditionFalse, Reason: string(StatusPending), Message: "No matching listeners"},
				{Type: CONDITION_TYPE_READY, Status: metav1.ConditionFalse, Reason: string(StatusPending), Message: "No matching listeners"},
			}},
		},
	}

	assert.Assert(t, binding.SetConfigured(nil))
	assert.Assert(t, !binding.Status.HasMatchingListener)
	assert.Assert(t, meta.FindStatusCondition(binding.Status.Conditions, CONDITION_TYPE_MATCHED) == nil)
	assert.Equal(t, meta.FindStatusCondition(binding.Status.Conditions, CONDITION_TYPE_READY).Status, metav1.ConditionTrue)
}

func TestConnectorReadyPreservesConfiguredFalseAndUnknown(t *testing.T) {
	for _, state := range []metav1.ConditionStatus{metav1.ConditionFalse, metav1.ConditionUnknown} {
		t.Run(string(state), func(t *testing.T) {
			connector := &Connector{Status: ConnectorStatus{Status: Status{Conditions: []metav1.Condition{
				{Type: CONDITION_TYPE_CONFIGURED, Status: state, Reason: string(StatusPending), Message: "Awaiting router observation"},
			}}}}

			assert.Assert(t, connector.ClearLegacyMatchingStatus())
			ready := meta.FindStatusCondition(connector.Status.Conditions, CONDITION_TYPE_READY)
			assert.Assert(t, ready != nil)
			assert.Equal(t, ready.Status, state)
		})
	}
}

func TestListenerMatchingStatusIsUnchanged(t *testing.T) {
	listener := &Listener{Status: ListenerStatus{Status: Status{Conditions: []metav1.Condition{}}}}

	assert.Assert(t, listener.SetHasMatchingConnector(true))
	assert.Assert(t, listener.Status.HasMatchingConnector)
	assert.Equal(t, meta.FindStatusCondition(listener.Status.Conditions, CONDITION_TYPE_MATCHED).Status, metav1.ConditionTrue)
}

func TestSiteClearsLegacyNetworkStatus(t *testing.T) {
	site := &Site{Status: SiteStatus{SitesInNetwork: 2, Network: []SiteRecord{}}}

	assert.Assert(t, site.ClearLegacyNetworkStatus())
	assert.Equal(t, site.Status.SitesInNetwork, 0)
	assert.Assert(t, site.Status.Network == nil)
	assert.Assert(t, !site.ClearLegacyNetworkStatus())
}
