//go:build integration

package controllerruntime

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/certs"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
)

// TestKindAccessBackends tests real API admission/defaulting and composition,
// not external reachability. The disposable controller must enable ingress,
// ingress-nginx, contour-http-proxy and gateway, with domains ingress.example,
// contour.example and gateway.example, Gateway class acceptance and port 8443.
// Install the official Contour HTTPProxy and Gateway/TLSRoute CRDs first. No
// ingress data plane is needed or simulated by this test.
func TestKindAccessBackends(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_CONTROL_TEST_KUBECONFIG")
	if kubeconfig == "" || os.Getenv("SKUPPER_CONTROL_TEST_ACCESS_BACKENDS") != "true" {
		t.Skip("set SKUPPER_CONTROL_TEST_KUBECONFIG and SKUPPER_CONTROL_TEST_ACCESS_BACKENDS=true for the configured disposable fixture")
	}
	rest, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := internalclient.NewClientFromRestConfig(rest, "skupper")
	if err != nil {
		t.Fatal(err)
	}
	kube, api := clients.GetKubeClient(), clients.GetSkupperClient().SkupperV2alpha1()
	dynamic := clients.GetDynamicClient()
	proxyGVR := schema.GroupVersionResource{Group: "projectcontour.io", Version: "v1", Resource: "httpproxies"}
	routeGVR := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1alpha2", Resource: "tlsroutes"}
	for _, generated := range []bool{false, true} {
		t.Run(fmt.Sprintf("generated=%t", generated), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			namespace := "access-live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			defer kube.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
			name, certificate := "access", "access-leaf"
			ports := []v2alpha1.SecuredAccessPort{{Name: "one", Port: 7443}, {Name: "two", Port: 9443}}
			if generated {
				name, certificate = "skupper-router", "skupper-site-server"
				ports = []v2alpha1.SecuredAccessPort{{Name: "edge", Port: 45671}, {Name: "inter-router", Port: 55671}}
				if _, err := api.Sites(namespace).Create(ctx, &v2alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site"}, Spec: v2alpha1.SiteSpec{LinkAccess: "ingress"}}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := api.Certificates(namespace).Create(ctx, &v2alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "issuer"}, Spec: v2alpha1.CertificateSpec{Subject: "acceptance issuer", Signing: true}}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := api.SecuredAccesses(namespace).Create(ctx, &v2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v2alpha1.SecuredAccessSpec{AccessType: "ingress", Selector: map[string]string{"app": "test"}, Ports: ports, Issuer: "issuer", Certificate: certificate}}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			poll := func(t *testing.T, check func(context.Context) error) {
				t.Helper()
				var last error
				err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
					last = check(ctx)
					return last == nil, nil
				})
				if err != nil {
					t.Fatalf("access did not converge: %v; last check: %v", err, last)
				}
			}
			sortEndpoints := func(endpoints []v2alpha1.Endpoint) {
				sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].Name < endpoints[j].Name })
			}
			var serviceIP string
			for _, backend := range []string{"ingress", "ingress-nginx", "contour-http-proxy", "gateway", "local"} {
				t.Run(backend, func(t *testing.T) {
					if generated {
						site, err := api.Sites(namespace).Get(ctx, "site", metav1.GetOptions{})
						if err != nil {
							t.Fatal(err)
						}
						site.Spec.LinkAccess = backend
						if _, err := api.Sites(namespace).Update(ctx, site, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
					} else {
						access, err := api.SecuredAccesses(namespace).Get(ctx, name, metav1.GetOptions{})
						if err != nil {
							t.Fatal(err)
						}
						access.Spec.AccessType = backend
						if _, err := api.SecuredAccesses(namespace).Update(ctx, access, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
					}
					expected := []v2alpha1.Endpoint{}
					for _, port := range ports {
						host, exposedPort := port.Name+"."+namespace+".ingress.example", "443"
						switch backend {
						case "contour-http-proxy":
							host = name + "-" + port.Name + "." + namespace + ".contour.example"
						case "gateway":
							host, exposedPort = name+"-"+port.Name+"."+namespace+".gateway.example", "8443"
						case "local":
							host, exposedPort = name+"."+namespace, strconv.Itoa(port.Port)
						}
						expected = append(expected, v2alpha1.Endpoint{Name: port.Name, Host: host, Port: exposedPort})
					}
					sortEndpoints(expected)
					var revisions map[string]string
					readBackends := func(ctx context.Context) (map[string]string, error) {
						result := map[string]string{}
						ingress, err := kube.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
						if backend == "ingress" || backend == "ingress-nginx" {
							if err != nil {
								return nil, err
							}
							if len(ingress.Spec.Rules) != len(ports) || (backend == "ingress-nginx" && ingress.Annotations["nginx.ingress.kubernetes.io/ssl-passthrough"] != "true") {
								return nil, fmt.Errorf("Ingress composition incomplete")
							}
							result["ingress"] = ingress.ResourceVersion
						} else if !apierrors.IsNotFound(err) {
							return nil, fmt.Errorf("obsolete Ingress remains: %v", err)
						}
						for _, gvr := range []schema.GroupVersionResource{proxyGVR, routeGVR} {
							for _, port := range ports {
								object, err := dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name+"-"+port.Name, metav1.GetOptions{})
								wanted := (backend == "contour-http-proxy" && gvr == proxyGVR) || (backend == "gateway" && gvr == routeGVR)
								if !wanted {
									if !apierrors.IsNotFound(err) {
										return nil, fmt.Errorf("obsolete %s remains: %v", gvr.Resource, err)
									}
									continue
								}
								if err != nil {
									return nil, err
								}
								if gvr == routeGVR {
									parents, _, _ := unstructured.NestedSlice(object.Object, "spec", "parentRefs")
									if len(parents) != 1 || parents[0].(map[string]interface{})["namespace"] != "skupper" {
										return nil, fmt.Errorf("TLSRoute lost cross-namespace Gateway parent")
									}
								}
								result[gvr.Resource+"/"+port.Name] = object.GetResourceVersion()
							}
						}
						return result, nil
					}
					poll(t, func(ctx context.Context) error {
						access, err := api.SecuredAccesses(namespace).Get(ctx, name, metav1.GetOptions{})
						if err != nil {
							return err
						}
						sortEndpoints(access.Status.Endpoints)
						if !access.IsReady() || !reflect.DeepEqual(access.Status.Endpoints, expected) {
							return fmt.Errorf("SecuredAccess status: %+v", access.Status)
						}
						service, err := kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
						if err != nil {
							return err
						}
						owner := metav1.GetControllerOf(service)
						if owner == nil || owner.UID != access.UID || service.Spec.ClusterIP == "" {
							return fmt.Errorf("Service ownership or allocation missing")
						}
						if serviceIP != "" && serviceIP != service.Spec.ClusterIP {
							return fmt.Errorf("Service allocation changed")
						}
						serviceIP = service.Spec.ClusterIP
						secret, err := kube.CoreV1().Secrets(namespace).Get(ctx, certificate, metav1.GetOptions{})
						if err != nil {
							return err
						}
						leaf, err := certs.DecodeCertificate(secret.Data[corev1.TLSCertKey])
						if err != nil {
							return fmt.Errorf("leaf is not a valid certificate")
						}
						for _, endpoint := range expected {
							if err := leaf.VerifyHostname(endpoint.Host); err != nil {
								return fmt.Errorf("leaf does not yet cover %s", endpoint.Host)
							}
						}
						if generated {
							site, err := api.Sites(namespace).Get(ctx, "site", metav1.GetOptions{})
							if err != nil {
								return err
							}
							grouped := append([]v2alpha1.Endpoint(nil), expected...)
							for i := range grouped {
								grouped[i].Group = "skupper-router"
							}
							sortEndpoints(site.Status.Endpoints)
							if !site.IsReady() || !reflect.DeepEqual(site.Status.Endpoints, grouped) {
								return fmt.Errorf("generated Site endpoints/status: %+v", site.Status)
							}
						}
						revisions, err = readBackends(ctx)
						return err
					})
					// Multiple refresh periods must leave API-defaulted backend objects
					// unchanged. This catches default-removal/update loops.
					time.Sleep(3 * time.Second)
					after, err := readBackends(ctx)
					if err != nil || !reflect.DeepEqual(revisions, after) {
						t.Fatalf("backend resourceVersions not stable: %v -> %v (%v)", revisions, after, err)
					}
					t.Log("exact endpoints and certificate SANs, Service allocation/ownership, obsolete backend retirement, and stable backend resourceVersions verified")
				})
			}
		})
	}
	gateway, err := dynamic.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}).Namespace("skupper").Get(context.Background(), "skupper", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statefulset, err := kube.AppsV1().StatefulSets("skupper").Get(context.Background(), "skupper-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner := metav1.GetControllerOf(gateway)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" || owner.UID != statefulset.UID {
		t.Fatal("shared Gateway does not belong to the stable controller StatefulSet")
	}
}
