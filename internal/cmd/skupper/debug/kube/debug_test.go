package kube

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/cmd/skupper/common"
	"github.com/skupperproject/skupper/internal/cmd/skupper/common/testutils"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	routerstatus "github.com/skupperproject/skupper/internal/routerstatus"
	"github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"github.com/spf13/cobra"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	v12 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	restclient "k8s.io/client-go/rest"
)

func TestCollectRouterStatus(t *testing.T) {
	doc := &routerstatus.Document{Version: 1, Router: routerstatus.Router{ID: "router1"}}
	encoded, err := routerstatus.Encode(doc)
	assert.NilError(t, err)
	statusCM := &v12.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Name:      "group1-status",
			Namespace: "test",
			Labels:    map[string]string{routerstatus.ConfigMapLabel: ""},
		},
		BinaryData: map[string][]byte{routerstatus.DataKey: encoded},
	}
	unrelatedCM := &v12.ConfigMap{ObjectMeta: v1.ObjectMeta{Name: "unrelated", Namespace: "test"}}
	cmd, ok := newCmdDebugWithMocks("test", []runtime.Object{statusCM, unrelatedCM}, nil, "")
	assert.Assert(t, ok)

	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	assert.NilError(t, collectRouterStatus(cmd, "resources/", tw))
	assert.NilError(t, tw.Close())

	files := readTarFiles(t, output.Bytes())
	assert.Assert(t, files["resources/Configmap-group1-status.yaml"] != nil)
	assert.Assert(t, files["resources/Configmap-group1-status.yaml.txt"] != nil)
	assert.Assert(t, files["resources/Configmap-unrelated.yaml"] == nil)
	var decoded routerstatus.Document
	assert.NilError(t, json.Unmarshal(files["resources/Configmap-group1-status-status.json"], &decoded))
	assert.Equal(t, decoded.Router.ID, doc.Router.ID)
}

func TestCollectRouterStatusWritesDecodeError(t *testing.T) {
	statusCM := &v12.ConfigMap{
		ObjectMeta: v1.ObjectMeta{Name: "bad-status", Namespace: "test", Labels: map[string]string{routerstatus.ConfigMapLabel: ""}},
		BinaryData: map[string][]byte{routerstatus.DataKey: []byte("not gzip")},
	}
	cmd, ok := newCmdDebugWithMocks("test", []runtime.Object{statusCM}, nil, "")
	assert.Assert(t, ok)

	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	assert.NilError(t, collectRouterStatus(cmd, "resources/", tw))
	assert.NilError(t, tw.Close())
	files := readTarFiles(t, output.Bytes())
	assert.Assert(t, files["resources/Configmap-bad-status.yaml"] != nil)
	assert.Assert(t, bytes.Contains(files["resources/Configmap-bad-status-status.json.error.txt"], []byte("failed to decode")))
}

func readTarFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		assert.NilError(t, err)
		files[header.Name], err = io.ReadAll(tr)
		assert.NilError(t, err)
	}
	return files
}

func TestCmdDebug_ValidateInput(t *testing.T) {
	type test struct {
		name                string
		args                []string
		flags               common.CommandDebugFlags
		k8sObjects          []runtime.Object
		skupperObjects      []runtime.Object
		skupperErrorMessage string
		expectedError       string
		restconfig          bool
	}

	testTable := []test{
		{
			name:          "too many args",
			flags:         common.CommandDebugFlags{},
			args:          []string{"test", "not-valid"},
			expectedError: "only one argument is allowed for this command",
			restconfig:    true,
		},
		{
			name:          "too many args",
			flags:         common.CommandDebugFlags{},
			args:          []string{""},
			expectedError: "filename must not be empty",
			restconfig:    true,
		},
		{
			name:          "rest empty",
			flags:         common.CommandDebugFlags{},
			args:          []string{"test"},
			expectedError: "failed setting up command",
			restconfig:    false,
		},
		{
			name:       "ok",
			flags:      common.CommandDebugFlags{},
			args:       []string{"test"},
			restconfig: true,
		},
		{
			name:       "ok default name",
			flags:      common.CommandDebugFlags{},
			args:       []string{},
			restconfig: true,
		},
	}

	for _, test := range testTable {
		t.Run(test.name, func(t *testing.T) {
			var rest restclient.Config
			cmd, err := newCmdDebugWithMocks("test", test.k8sObjects, test.skupperObjects, "")
			assert.Assert(t, err)
			if test.restconfig {
				cmd.Rest = &rest
			}
			cmd.Flags = &test.flags

			testutils.CheckValidateInput(t, cmd, test.expectedError, test.args)

		})
	}
}

func TestCmdDebug_InputToOptions(t *testing.T) {
	type test struct {
		name                string
		namespace           string
		filename            string
		args                []string
		flags               common.CommandDebugFlags
		k8sObjects          []runtime.Object
		skupperObjects      []runtime.Object
		skupperErrorMessage string
		expectedName        string
	}

	testTable := []test{
		{
			name:      "default name",
			namespace: "default",
			filename:  "skupper-dump",
			args:      []string{},
			flags:     common.CommandDebugFlags{},
		},
		{
			name:      "name",
			namespace: "test",
			filename:  "dump",
			args:      []string{},
			flags:     common.CommandDebugFlags{},
		},
	}

	for _, test := range testTable {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := newCmdDebugWithMocks("test", test.k8sObjects, test.skupperObjects, "")
			assert.Assert(t, err)
			cmd.Flags = &test.flags
			cmd.Namespace = test.namespace
			cmd.fileName = test.filename
			name := fmt.Sprintf("%s-%s-%s", test.filename, cmd.Namespace, time.Now().Format("20060102150405"))
			cmd.InputToOptions()

			assert.Check(t, cmd.fileName == name)
		})
	}
}

func TestCmdDebug_Run(t *testing.T) {
	type test struct {
		name                string
		DebugName           string
		flags               common.CommandDebugFlags
		k8sObjects          []runtime.Object
		skupperObjects      []runtime.Object
		skupperErrorMessage string
		errorMessage        string
	}

	testTable := []test{
		{
			name:  "default",
			flags: common.CommandDebugFlags{},
			k8sObjects: []runtime.Object{
				&appsv1.Deployment{
					TypeMeta: v1.TypeMeta{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-controller",
						Namespace: "test",
						Labels: map[string]string{
							"application": "skupper-controller",
						},
					},
					Spec: appsv1.DeploymentSpec{
						Selector: &v1.LabelSelector{
							MatchLabels: map[string]string{
								"application": "skupper-controller",
							},
						},
						Template: v12.PodTemplateSpec{
							ObjectMeta: v1.ObjectMeta{
								Name:      "skupper-controller",
								Namespace: "test",
								Labels: map[string]string{
									"application": "skupper-controller",
								},
							},
						},
					},
				},
				&appsv1.Deployment{
					TypeMeta: v1.TypeMeta{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-router",
						Namespace: "test",
						Labels: map[string]string{
							"application": "skupper-router",
						},
					},
					Spec: appsv1.DeploymentSpec{
						Selector: &v1.LabelSelector{
							MatchLabels: map[string]string{
								"application": "skupper-router",
							},
						},
						Template: v12.PodTemplateSpec{
							ObjectMeta: v1.ObjectMeta{
								Name:      "skupper-router",
								Namespace: "test",
								Labels: map[string]string{
									"application": "skupper-router",
								},
							},
						},
					},
				},
				&v12.Pod{
					TypeMeta: v1.TypeMeta{
						APIVersion: "apps/v1",
						Kind:       "Pod",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-router-cbbd7c69c-9dc55",
						Namespace: "test",
						Labels: map[string]string{
							"application": "skupper-router",
						},
					},
					Spec: v12.PodSpec{
						Containers: []v12.Container{
							{
								Name: "router",
							},
						},
					},
				},
				&v12.Pod{
					TypeMeta: v1.TypeMeta{
						APIVersion: "apps/v1",
						Kind:       "Pod",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-controller-cbbd7c69c-9dc55",
						Namespace: "test",
						Labels: map[string]string{
							"application": "skupper-controller",
						},
					},
					Spec: v12.PodSpec{
						Containers: []v12.Container{
							{
								Name: "controller",
							},
						},
					},
				},
				&v12.Service{
					TypeMeta: v1.TypeMeta{
						APIVersion: "apps/v1",
						Kind:       "Service",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-grant-server",
						Namespace: "test",
					},
				},
				&v12.Service{
					TypeMeta: v1.TypeMeta{
						APIVersion: "apps/v1",
						Kind:       "Service",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-router",
						Namespace: "test",
					},
				},
				&v12.ConfigMap{
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-router",
						Namespace: "test",
					},
				},
				&v12.Secret{
					TypeMeta: v1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Secret",
					},
				},
				&appsv1.ReplicaSet{
					TypeMeta: v1.TypeMeta{
						APIVersion: "v1",
						Kind:       "ReplicaSet",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "replicaset-12345-123",
						Namespace: "test",
						Labels: map[string]string{
							"app.kubernetes.io/name": "skupper-router",
						},
					},
				},
				&rbacv1.Role{
					TypeMeta: v1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Role",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-router",
						Namespace: "test",
					},
				},
				&rbacv1.RoleBinding{
					TypeMeta: v1.TypeMeta{
						APIVersion: "v1",
						Kind:       "RoleBinding",
					},
					ObjectMeta: v1.ObjectMeta{
						Name:      "skupper-router",
						Namespace: "test",
					},
				},
			},
			skupperObjects: []runtime.Object{
				&v2alpha1.AccessGrant{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-token",
						Namespace: "test",
					},
				},
				&v2alpha1.AccessToken{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-token",
						Namespace: "test",
					},
				},
				&v2alpha1.Connector{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-connector",
						Namespace: "test",
					},
				},
				&v2alpha1.Listener{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-listener",
						Namespace: "test",
					},
				},
				&v2alpha1.Site{
					ObjectMeta: v1.ObjectMeta{
						Name:      "the-site",
						Namespace: "test",
					},
				},
				&v2alpha1.Certificate{
					ObjectMeta: v1.ObjectMeta{
						Name:      "link-test",
						Namespace: "test",
					},
				},
				&v2alpha1.Link{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-link",
						Namespace: "test",
					},
				},
				&v2alpha1.AttachedConnectorBinding{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-attachedConnectorBinding",
						Namespace: "test",
					},
				},
				&v2alpha1.AttachedConnector{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-attachedConnector",
						Namespace: "test",
					},
				},
				&v2alpha1.RouterAccess{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-routerAccess",
						Namespace: "test",
					},
				},
				&v2alpha1.SecuredAccess{
					ObjectMeta: v1.ObjectMeta{
						Name:      "my-securedAccess",
						Namespace: "test",
					},
				},
			},
		},
	}

	for _, test := range testTable {
		cmd, err := newCmdDebugWithMocks("test", test.k8sObjects, test.skupperObjects, test.skupperErrorMessage)
		assert.Assert(t, err)
		cmd.Flags = &test.flags
		cmd.Namespace = "test"
		cmd.fileName = "/tmp/test"
		cmd.CobraCmd = &cobra.Command{Use: "test"}
		defer os.Remove("/tmp/test.tar.gz") //clean up
		t.Run(test.name, func(t *testing.T) {

			err := cmd.Run()
			if err != nil {
				assert.Check(t, test.errorMessage == err.Error())
			} else {
				assert.Check(t, err == nil)
			}
		})
	}
}

// --- helper methods

func newCmdDebugWithMocks(namespace string, k8sObjects []runtime.Object, skupperObjects []runtime.Object, fakeSkupperError string) (*CmdDebug, error) {

	client, err := fakeclient.NewFakeClient(namespace, k8sObjects, skupperObjects, fakeSkupperError)
	if err != nil {
		return nil, err
	}
	cmdDebug := &CmdDebug{
		Client:     client.GetSkupperClient().SkupperV2alpha1(),
		KubeClient: client.GetKubeClient(),
		Namespace:  namespace,
	}

	return cmdDebug, nil
}
