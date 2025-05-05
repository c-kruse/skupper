package site

import (
	"testing"

	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestUpdateSecretChecksum(t *testing.T) {
	var sum [32]byte
	secret := corev1.Secret{
		Data: map[string][]byte{
			"a": []byte("first"),
			"b": []byte("second"),
			"c": []byte("third"),
		},
	}
	assert.Check(t, updateSecretChecksum(&secret, &sum), "expected initial difference in sum")
	for i := range 10 {
		secret.Data = map[string][]byte{
			"a": []byte("first"),
			"b": []byte("second"),
			"c": []byte("third"),
		}
		assert.Check(t, !updateSecretChecksum(&secret, &sum), "expected no change to sum, round %d", i)

	}
	assert.Check(t, !updateSecretChecksum(&corev1.Secret{
		Data: map[string][]byte{"a": []byte("first"), "c": []byte("third"), "b": []byte("second")},
	}, &sum), "expect sum not to change with identical data from different map instance")
	assert.Check(t, updateSecretChecksum(&corev1.Secret{
		Data: map[string][]byte{"test": nil, "a": []byte("first"), "c": []byte("third"), "b": []byte("second")},
	}, &sum), "expect sum to change with different map key")
	assert.Check(t, updateSecretChecksum(&secret, &sum), "expect to sum removing map key")
	assert.Check(t, updateSecretChecksum(&corev1.Secret{
		Data: map[string][]byte{"a": []byte("!first"), "c": []byte("third"), "b": []byte("second")},
	}, &sum), "expect sum to change changing data values")
	assert.Check(t, updateSecretChecksum(&secret, &sum))
}
