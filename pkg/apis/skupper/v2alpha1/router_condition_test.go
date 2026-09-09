package v2alpha1

import (
	"testing"

	"gotest.tools/v3/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRouterManagementFailureConditionsRecover(t *testing.T) {
	link := &Link{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	link.SetConfigured(nil)
	assert.Assert(t, link.SetOperationalCondition(false, "connection refused", "remote", "east"))
	condition := meta.FindStatusCondition(link.Status.Conditions, CONDITION_TYPE_OPERATIONAL)
	assert.Equal(t, condition.Reason, string(StatusError))
	assert.Equal(t, condition.Message, "connection refused")
	assert.Assert(t, !link.SetOperationalCondition(false, "connection refused", "remote", "east"), "unchanged failures must not trigger another status write")
	assert.Assert(t, link.SetOperationalCondition(true, "", "remote", "east"))
	condition = meta.FindStatusCondition(link.Status.Conditions, CONDITION_TYPE_OPERATIONAL)
	assert.Equal(t, condition.Status, metav1.ConditionTrue)
	assert.Equal(t, condition.Message, STATUS_OK)

	listener := &Listener{ObjectMeta: metav1.ObjectMeta{Generation: 4}}
	listener.SetConfigured(nil)
	listener.SetHasMatchingConnector(false)
	assert.Assert(t, listener.SetMatchingCondition(false, "listener management failed"))
	condition = meta.FindStatusCondition(listener.Status.Conditions, CONDITION_TYPE_MATCHED)
	assert.Equal(t, condition.Reason, string(StatusError))
	assert.Equal(t, condition.Message, "listener management failed")
	assert.Assert(t, !listener.SetMatchingCondition(false, "listener management failed"), "unchanged failures must not trigger another status write")
	assert.Assert(t, listener.SetHasMatchingConnector(true))
	listener.SetMatchingCondition(true, "")
	condition = meta.FindStatusCondition(listener.Status.Conditions, CONDITION_TYPE_MATCHED)
	assert.Equal(t, condition.Status, metav1.ConditionTrue)
	assert.Equal(t, condition.Message, STATUS_OK)
}

func TestLocalOnlyMatchingStatusCleared(t *testing.T) {
	connector := &Connector{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	connector.SetConfigured(nil)
	connector.SetHasMatchingListener(true)
	assert.Assert(t, connector.ClearMatchingStatus())
	assert.Equal(t, connector.Status.HasMatchingListener, false)
	assert.Assert(t, meta.FindStatusCondition(connector.Status.Conditions, CONDITION_TYPE_MATCHED) == nil)
	assert.Equal(t, connector.Status.StatusType, StatusReady)

	binding := &AttachedConnectorBinding{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	binding.SetConfigured(nil)
	binding.SetHasMatchingListener(true)
	assert.Assert(t, binding.ClearMatchingStatus())
	assert.Equal(t, binding.Status.HasMatchingListener, false)
	assert.Assert(t, meta.FindStatusCondition(binding.Status.Conditions, CONDITION_TYPE_MATCHED) == nil)
	assert.Equal(t, binding.Status.StatusType, StatusReady)
}
