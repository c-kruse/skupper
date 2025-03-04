package cmd

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type mockSecretCreator func(secret *corev1.Secret) (*corev1.Secret, error)

func (fn mockSecretCreator) Create(_ context.Context, secret *corev1.Secret, _ metav1.CreateOptions) (*corev1.Secret, error) {
	return fn(secret)
}

func TestEnsureCookieSecret(t *testing.T) {

	tCtx := context.Background()
	testCases := []struct {
		ArgClient     secretCreator
		ArgSecretName string

		ExpectErr bool
		Assert    func(t *testing.T, client secretCreator)
	}{
		{
			ArgClient:     fake.NewClientset().CoreV1().Secrets("testing"),
			ArgSecretName: "mysecret",
			Assert: func(t *testing.T, client secretCreator) {
				s, err := client.(v1.SecretInterface).Get(tCtx, "mysecret", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				val := s.Data["secret"]
				if len(val) < 42 {
					t.Errorf("Expected secret to have secret key with sufficeint length: %q", val)
				}
			},
		}, {
			ArgClient: fake.NewClientset(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mysecret",
					Namespace: "testing",
				},
				Type: "Opaque",
				Data: map[string][]byte{
					"secret": []byte("expected"),
				},
			}).CoreV1().Secrets("testing"),
			ArgSecretName: "mysecret",
			Assert: func(t *testing.T, client secretCreator) {
				s, err := client.(v1.SecretInterface).Get(tCtx, "mysecret", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				val := string(s.Data["secret"])
				if val != "expected" {
					t.Errorf("Expected secret to not change: wanted 'expected' got %q", val)
				}
			},
		}, {
			ArgClient: fake.NewClientset(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mysecret",
					Namespace: "testing",
				},
				Type: "Opaque",
				Data: map[string][]byte{
					"secret": []byte("expected"),
				},
			}).CoreV1().Secrets("testing"),
			ArgSecretName: "secret-ii",
		}, {
			ArgClient: mockSecretCreator(func(*corev1.Secret) (*corev1.Secret, error) {
				return nil, fmt.Errorf("500 server error")
			}),
			ArgSecretName: "mysecret",
			ExpectErr:     true,
		}, {
			ArgClient: mockSecretCreator(func(*corev1.Secret) (*corev1.Secret, error) {
				return nil, errors.NewAlreadyExists(schema.GroupResource{}, "already exists")
			}),
			ArgSecretName: "mysecret",
			ExpectErr:     false,
		},
	}

	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			err := ensureCookieSecret(tCtx, tc.ArgClient, tc.ArgSecretName)
			switch {
			case err == nil && !tc.ExpectErr:
				// OKAY
			case err == nil && tc.ExpectErr:
				t.Fatal("Expected error but returned nil")
			case err != nil && tc.ExpectErr:
				// OKAY
			case err != nil && !tc.ExpectErr:
				t.Fatalf("Unxpected error %s", err)
			}

			if tc.Assert != nil {
				tc.Assert(t, tc.ArgClient)
			}

		})
	}
}
