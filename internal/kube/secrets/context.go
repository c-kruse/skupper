package secrets

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
)

const (
	AnnotationKeyTlsPriorValidRevisions = "skupper.io/tls-prior-valid-revisions"

	annotationKeySslProfileContext = "internal.skupper.io/tls-profile-context"
)

type secretContext struct {
	ProfileName string `json:"profileName"`
	Ordinal     uint64 `json:"ordinal"`
}

func fromSecret(secret *corev1.Secret) (secretContext, bool, error) {
	var context secretContext
	if secret == nil || secret.Annotations == nil {
		return context, false, nil
	}
	val, ok := secret.Annotations[annotationKeySslProfileContext]
	if !ok {
		return context, false, nil
	}
	return context, true, json.Unmarshal([]byte(val), &context)
}

func updateSecret(secret *corev1.Secret, context secretContext) (bool, error) {
	prev, _, _ := fromSecret(secret)
	if prev == context {
		return false, nil
	}
	raw, err := json.Marshal(context)
	if err != nil {
		return true, err
	}
	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations[annotationKeySslProfileContext] = string(raw)
	return true, nil
}
