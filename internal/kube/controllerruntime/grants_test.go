package controllerruntime

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/skupperproject/skupper/internal/certs"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/controller"
	"github.com/skupperproject/skupper/internal/kube/leadership"
	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestGrantLookupRejectsStaleAssignmentAndSite(t *testing.T) {
	ctx := context.Background()
	site := &v2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "west", UID: "original"}}
	clients, err := fakeclient.NewFakeClient("control", nil, []runtime.Object{site}, "")
	if err != nil {
		t.Fatal(err)
	}
	config := &controller.Config{Namespace: "control", Name: "skupper-controller"}
	cached := site.DeepCopy()
	lookup := func(string) (*v2alpha1.Site, bool) { return cached, true }
	if _, err := lookupGrantSite(ctx, clients, config, lookup, "tenant"); err != nil {
		t.Fatal(err)
	}
	site.UID = "replacement"
	if _, err := clients.GetSkupperClient().SkupperV2alpha1().Sites("tenant").Update(ctx, site, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lookupGrantSite(ctx, clients, config, lookup, "tenant"); err == nil {
		t.Fatal("stale cached Site authorized")
	}
	cached = site.DeepCopy()
	assignment := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "skupper"}, Data: map[string]string{"controller": "other/controller"}}
	if _, err := clients.GetKubeClient().CoreV1().ConfigMaps("tenant").Create(ctx, assignment, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lookupGrantSite(ctx, clients, config, lookup, "tenant"); err == nil {
		t.Fatal("namespace reassignment did not revoke grant authority")
	}
}

func TestGrantGeneratorRechecksBeforeReleasingCredentials(t *testing.T) {
	ca, err := certs.GenerateSecret("skupper-site-ca", "test CA", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ca.Namespace = "tenant"
	clients, err := fakeclient.NewFakeClient("control", []runtime.Object{ca}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"valid", "standby", "leader lost", "Site replaced"} {
		t.Run(test, func(t *testing.T) {
			fence := leadership.NewFence()
			var active atomic.Pointer[leadership.Fence]
			if test != "standby" {
				fence.Open()
				active.Store(fence)
			}
			site := &v2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "west", UID: "original", ResourceVersion: "7"}}
			site.Status.Endpoints = []v2alpha1.Endpoint{{Name: "inter-router", Host: "router.example", Port: "55671"}}
			lookups := 0
			lookup := func(context.Context, string) (*v2alpha1.Site, error) {
				lookups++
				result := site.DeepCopy()
				if lookups == 2 {
					switch test {
					case "leader lost":
						fence.Close()
					case "Site replaced":
						result.UID = "replacement"
					}
				}
				return result, nil
			}
			var output bytes.Buffer
			err := grantGenerator(context.Background(), clients, lookup, &active)("tenant", "link", "remote-site", &output)
			if test == "valid" {
				if err != nil || !strings.Contains(output.String(), "kind: Link") || !strings.Contains(output.String(), "kind: Secret") {
					t.Fatalf("valid grant did not contain Secret and Link: %v", err)
				}
			} else if err == nil || output.Len() != 0 {
				t.Fatalf("unauthorized credentials released: err=%v bytes=%d", err, output.Len())
			}
		})
	}
}
