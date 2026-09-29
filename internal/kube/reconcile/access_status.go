package reconcile

import (
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func deriveStandaloneAccessStatuses(snapshot Snapshot, desired *DesiredNamespace) {
	desired.Statuses.SourceNamespaces = map[string]types.UID{snapshot.Namespace.Name: snapshot.Namespace.UID}
	evaluationTime := snapshot.EvaluationTime
	if evaluationTime.IsZero() {
		evaluationTime = time.Unix(1, 0).UTC()
	}
	now := metav1.NewTime(evaluationTime)
	accesses := append([]*skupperv2alpha1.SecuredAccess(nil), snapshot.SecuredAccesses...)
	sort.Slice(accesses, func(i, j int) bool { return accesses[i].Name < accesses[j].Name })
	for _, current := range accesses {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		configured, resolved, endpoints := securedAccessStatus(snapshot, updated)
		updated.Status.Endpoints = endpoints
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, configured, updated.Generation, now)
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_RESOLVED, resolved, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_CONFIGURED, skupperv2alpha1.CONDITION_TYPE_RESOLVED)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.SecuredAccesses = append(desired.Statuses.SecuredAccesses, updated)
		}
	}
	secrets := map[string]*corev1.Secret{}
	for _, secret := range snapshot.Secrets {
		secrets[secret.Name] = secret
	}
	for _, current := range snapshot.Certificates {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		state, expiration := certificateState(updated, secrets[updated.Name], evaluationTime)
		updated.Status.Expiration = expiration
		setStatusCondition(&updated.Status.Status, skupperv2alpha1.CONDITION_TYPE_READY, state, updated.Generation, now)
		aggregateStatus(&updated.Status.Status, updated.Generation, now, skupperv2alpha1.CONDITION_TYPE_READY)
		if !reflect.DeepEqual(*before, updated.Status) {
			desired.Statuses.Certificates = append(desired.Statuses.Certificates, updated)
		}
	}
}
