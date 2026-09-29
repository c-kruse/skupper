package reconcile

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/skupperproject/skupper/internal/kube/certificates"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func TestAccessCompositionWaitsForRealParentUIDAndThenCreatesHAChildren(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Sites[0].Spec.LinkAccess = "loadbalancer"
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.GeneratedAccess == nil || desired.GeneratedAccess.Name != "skupper-router" {
		t.Fatalf("default RouterAccess was not derived: %#v", desired.GeneratedAccess)
	}
	if len(desired.SecuredAccesses) != 0 {
		t.Fatalf("children were derived with a fabricated parent UID: %#v", desired.SecuredAccesses)
	}
	for _, intent := range desired.Intents {
		if len(intent.RouterListeners) != 0 {
			t.Fatalf("RouterAccess without a UID contributed router resources: %#v", intent.RouterListeners)
		}
	}

	access := desired.GeneratedAccess.DeepCopy()
	access.UID = "access-uid"
	snapshot.RouterAccesses = []*skupperv2alpha1.RouterAccess{access}
	snapshot.Sites[0].Spec.HA = true
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if got := []string{desired.SecuredAccesses[0].Name, desired.SecuredAccesses[1].Name}; !cmp.Equal(got, []string{"skupper-router", "skupper-router-2"}) {
		t.Fatalf("unexpected HA SecuredAccess names: %v", got)
	}
	if desired.SecuredAccesses[0].Spec.Selector["skupper.io/group"] != "skupper-router" || desired.SecuredAccesses[1].Spec.Selector["skupper.io/group"] != "skupper-router-2" {
		t.Fatalf("HA selectors do not isolate router groups: %#v", desired.SecuredAccesses)
	}
	for _, secured := range desired.SecuredAccesses {
		if len(secured.OwnerReferences) != 1 || secured.OwnerReferences[0].UID != access.UID || secured.OwnerReferences[0].Controller == nil || !*secured.OwnerReferences[0].Controller {
			t.Fatalf("generated SecuredAccess is not controller-owned by RouterAccess: %#v", secured.OwnerReferences)
		}
	}
}

func TestStandaloneAccessAndCertificateDeriveWithoutSite(t *testing.T) {
	snapshot := Snapshot{Namespace: NamespaceIdentity{Name: "controller-ns", UID: "namespace-uid"}, Assignment: Assignment{Controller: "controller-ns/skupper-controller", Controlled: true}, DefaultAccessType: "local"}
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{{ObjectMeta: metav1.ObjectMeta{Name: "enrollment", Namespace: "controller-ns", UID: "access-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{Selector: map[string]string{"app": "controller"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 443, TargetPort: 8443, Protocol: "TCP"}}, Certificate: "enrollment", Issuer: "issuer"}}}
	snapshot.Certificates = []*skupperv2alpha1.Certificate{{ObjectMeta: metav1.ObjectMeta{Name: "issuer", Namespace: "controller-ns", UID: "issuer-uid"}, Spec: skupperv2alpha1.CertificateSpec{Subject: "issuer", Signing: true}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.Site != nil || len(desired.Intents) != 0 {
		t.Fatalf("standalone derivation fabricated Site/router intent: %#v", desired)
	}
	if len(desired.AccessServices) != 1 || desired.AccessServices[0].Name != "enrollment" {
		t.Fatalf("standalone SecuredAccess Service was not derived: %#v", desired.AccessServices)
	}
	if len(desired.Certificates) != 1 || desired.Certificates[0].Name != "enrollment" {
		t.Fatalf("standalone SecuredAccess Certificate request was not derived: %#v", desired.Certificates)
	}
	if len(desired.Statuses.SecuredAccesses) != 1 || len(desired.Statuses.Certificates) != 1 {
		t.Fatalf("standalone status was not projected without a Site: %#v", desired.Statuses)
	}
}

func TestCertificateStatusIncludesSummaryAndExpiration(t *testing.T) {
	certificate := &skupperv2alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "issuer", Namespace: "controller-ns", UID: "issuer-uid", Generation: 2}, Spec: skupperv2alpha1.CertificateSpec{Subject: "issuer", Signing: true}}
	secret, err := certificates.GenerateSecret(certificate, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Namespace: NamespaceIdentity{Name: "controller-ns", UID: "namespace-uid"}, Assignment: Assignment{Controller: "controller-ns/skupper-controller", Controlled: true}, Certificates: []*skupperv2alpha1.Certificate{certificate}, Secrets: []*corev1.Secret{secret}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.Certificates) != 1 {
		t.Fatalf("Certificate status was not projected: %#v", desired.Statuses.Certificates)
	}
	status := desired.Statuses.Certificates[0].Status
	if status.StatusType != skupperv2alpha1.StatusReady || status.Message != "OK" || status.Expiration == "" {
		t.Fatalf("Certificate columns were not populated: %#v", status)
	}
}

func TestStandaloneSecuredAccessGetsServiceAndSharedCertificateUnion(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.DefaultAccessType = "loadbalancer"
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{
		{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "site", UID: "one-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{Selector: map[string]string{"app": "one"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 443, TargetPort: 8443, Protocol: "TCP"}}, Certificate: "shared", Issuer: "skupper-site-ca"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "site", UID: "two-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{AccessType: "local", Selector: map[string]string{"app": "two"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 444, TargetPort: 8444, Protocol: "TCP"}}, Certificate: "shared", Issuer: "skupper-site-ca"}},
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.AccessServices) != 2 || desired.AccessServices[0].Spec.Type != "LoadBalancer" || desired.AccessServices[1].Spec.Type != "ClusterIP" {
		t.Fatalf("standalone access Services were not derived using explicit/default types: %#v", desired.AccessServices)
	}
	var shared *skupperv2alpha1.Certificate
	for _, certificate := range desired.Certificates {
		if certificate.Name == "shared" {
			shared = certificate
		}
	}
	if shared == nil || len(shared.OwnerReferences) != 2 || !cmp.Equal(shared.Spec.Hosts, []string{"one", "one.site", "two", "two.site"}) {
		t.Fatalf("shared certificate did not merge owners and hosts: %#v", shared)
	}
}

func TestDisabledLinkAccessRetiresControlledDefaultFromEffectiveIntent(t *testing.T) {
	snapshot := baseSnapshot()
	access := defaultRouterAccess(snapshot.Sites[0], nil)
	access.UID = "old-access"
	snapshot.RouterAccesses = []*skupperv2alpha1.RouterAccess{access}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if desired.GeneratedAccess != nil {
		t.Fatalf("disabled link access retained generated RouterAccess: %#v", desired.GeneratedAccess)
	}
	for _, intent := range desired.Intents {
		if len(intent.RouterListeners) != 0 {
			t.Fatalf("retiring default RouterAccess remained in published intent: %#v", intent.RouterListeners)
		}
	}
}

func TestHASecondaryConnectsToPrimaryThroughGeneratedInterRouterAccess(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Sites[0].Spec.HA = true
	snapshot.Sites[0].Spec.LinkAccess = "loadbalancer"
	access := defaultRouterAccess(snapshot.Sites[0], nil)
	access.UID = "access-uid"
	snapshot.RouterAccesses = []*skupperv2alpha1.RouterAccess{access}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	primary := desired.Intents[RouterTarget{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"}]
	secondary := desired.Intents[RouterTarget{NamespaceUID: "namespace-uid", SiteUID: "site-uid", RouterGroup: "skupper-router-2"}]
	if len(primary.RouterConnections) != 0 {
		t.Fatalf("primary unexpectedly connects back to itself: %#v", primary.RouterConnections)
	}
	if len(secondary.RouterConnections) != 1 {
		t.Fatalf("secondary has no local inter-router connection: %#v", secondary.RouterConnections)
	}
	connection := secondary.RouterConnections[0]
	if connection.Host != "skupper-router" || connection.Port != 55671 || connection.Role != "inter-router" || connection.TLS.Mode != "mutual" {
		t.Fatalf("unexpected local inter-router connection: %#v", connection)
	}
	if len(secondary.CredentialBindings) != 1 || !cmp.Equal(secondary.CredentialBindings[0].Usages, []string{"client-auth", "server-auth", "trust"}) {
		t.Fatalf("local listener/connector credentials were not merged: %#v", secondary.CredentialBindings)
	}
}

func TestSiteProjectsOnlyOwnedGeneratedRouterAccessEndpointsAndClearsRemoved(t *testing.T) {
	snapshot := baseSnapshot()
	site := snapshot.Sites[0]
	site.Spec.LinkAccess = "local"
	site.Status.Endpoints = []skupperv2alpha1.Endpoint{{Name: "stale", Host: "removed"}}
	controller := true
	routerAccess := defaultRouterAccess(site, nil)
	routerAccess.UID = "router-access-uid"
	snapshot.RouterAccesses = []*skupperv2alpha1.RouterAccess{routerAccess}
	secured := desiredSecuredAccess("site", "skupper-router", "skupper-router", routerAccess, site.DefaultIssuer())
	secured.UID = "secured-uid"
	foreign := secured.DeepCopy()
	foreign.Name, foreign.UID = "foreign", "foreign-uid"
	foreign.OwnerReferences[0].UID = "replaced-router-access"
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{foreign, secured}
	snapshot.Services = []*corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Name: secured.Name, Namespace: "site", OwnerReferences: []metav1.OwnerReference{{UID: secured.UID, Controller: &controller}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: foreign.Name, Namespace: "site", OwnerReferences: []metav1.OwnerReference{{UID: foreign.UID, Controller: &controller}}}},
	}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.Sites) != 1 {
		t.Fatalf("Site endpoint status was not projected: %#v", desired.Statuses.Sites)
	}
	want := []skupperv2alpha1.Endpoint{{Name: "edge", Host: "skupper-router.site", Port: "45671", Group: "skupper-router"}, {Name: "inter-router", Host: "skupper-router.site", Port: "55671", Group: "skupper-router"}}
	if diff := cmp.Diff(want, desired.Statuses.Sites[0].Status.Endpoints); diff != "" {
		t.Fatalf("Site endpoint mismatch (-want +got):\n%s", diff)
	}

	snapshot.SecuredAccesses = nil
	snapshot.Services = nil
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.Sites) != 1 || len(desired.Statuses.Sites[0].Status.Endpoints) != 0 {
		t.Fatalf("removed endpoints were retained: %#v", desired.Statuses.Sites)
	}
}

func TestRouteEndpointProjectsToStandaloneSecuredAccessStatus(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.DefaultAccessType = "route"
	secured := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "site", UID: "secured-uid", Generation: 3}, Spec: skupperv2alpha1.SecuredAccessSpec{Selector: map[string]string{"app": "router"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "inter-router", Port: 55671, TargetPort: 55671, Protocol: "TCP"}}}}
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{secured}
	controller, block := true, true
	snapshot.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: secured.Name, Namespace: "site", OwnerReferences: []metav1.OwnerReference{{UID: secured.UID, Controller: &controller, BlockOwnerDeletion: &block}}}}}
	snapshot.Routes = []*routev1.Route{{ObjectMeta: metav1.ObjectMeta{Name: "external-inter-router", Namespace: "site", OwnerReferences: []metav1.OwnerReference{{UID: secured.UID, Controller: &controller}}}, Status: routev1.RouteStatus{Ingress: []routev1.RouteIngress{{Host: "external.apps.example"}}}}}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.SecuredAccesses) != 1 {
		t.Fatalf("SecuredAccess status was not projected: %#v", desired.Statuses.SecuredAccesses)
	}
	status := desired.Statuses.SecuredAccesses[0].Status
	if diff := cmp.Diff([]skupperv2alpha1.Endpoint{{Name: "inter-router", Host: "external.apps.example", Port: "443"}}, status.Endpoints); diff != "" {
		t.Fatalf("route endpoint mismatch (-want +got):\n%s", diff)
	}
	if !desired.Statuses.SecuredAccesses[0].IsReady() {
		t.Fatalf("resolved standalone SecuredAccess is not Ready: %#v", status.Conditions)
	}
}

func TestIngressNginxUsesConfiguredDomainClassAndProjectsEndpoint(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.DefaultAccessType = "ingress-nginx"
	snapshot.AccessConfig = AccessConfig{IngressDomain: "apps.example", IngressClassName: "public-nginx"}
	secured := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "site", UID: "secured-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{Selector: map[string]string{"app": "router"}, Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "edge", Port: 45671, TargetPort: 45671, Protocol: "TCP"}}}}
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{secured}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.AccessIngresses) != 1 {
		t.Fatalf("Ingress was not derived: %#v", desired.AccessIngresses)
	}
	ingress := desired.AccessIngresses[0]
	if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != "public-nginx" || ingress.Spec.Rules[0].Host != "edge.site.apps.example" || ingress.Annotations["nginx.ingress.kubernetes.io/ssl-passthrough"] != "true" {
		t.Fatalf("unexpected nginx Ingress: %#v", ingress)
	}
	controller := true
	snapshot.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: secured.Name, Namespace: "site", OwnerReferences: []metav1.OwnerReference{{UID: secured.UID, Controller: &controller}}}}}
	snapshot.Ingresses = desired.AccessIngresses
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.SecuredAccesses) != 1 || !cmp.Equal(desired.Statuses.SecuredAccesses[0].Status.Endpoints, []skupperv2alpha1.Endpoint{{Name: "edge", Host: "edge.site.apps.example", Port: "443"}}) {
		t.Fatalf("Ingress endpoint was not projected: %#v", desired.Statuses.SecuredAccesses)
	}
}

func TestSecuredAccessDefaultsServicePortAndWaitsForIngressDomain(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.DefaultAccessType = "ingress"
	secured := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "site", UID: "secured-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 443}}}}
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{secured}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	port := desired.AccessServices[0].Spec.Ports[0]
	if port.Protocol != corev1.ProtocolTCP || port.TargetPort.IntValue() != 443 {
		t.Fatalf("Service defaults were not normalized: %#v", port)
	}
	controller := true
	snapshot.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: secured.Name, OwnerReferences: []metav1.OwnerReference{{UID: secured.UID, Controller: &controller}}}}}
	snapshot.Ingresses = desired.AccessIngresses
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.SecuredAccesses) != 1 || len(desired.Statuses.SecuredAccesses[0].Status.Endpoints) != 0 {
		t.Fatalf("Ingress endpoint resolved before a domain was configured or inferred: %#v", desired.Statuses.SecuredAccesses)
	}
}

func TestDynamicAccessBackendsAndEnabledTypes(t *testing.T) {
	snapshot := baseSnapshot()
	secured := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "site", UID: "secured-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{AccessType: "contour-http-proxy", Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 443}}}}
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{secured}
	snapshot.AccessConfig = AccessConfig{EnabledTypes: []string{"local"}, HTTPProxyDomain: "apps.example"}
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.AccessServices) != 0 || len(desired.AccessHTTPProxies) != 0 || len(desired.Diagnostics) == 0 {
		t.Fatalf("disabled explicit access type was realized: %#v", desired)
	}
	snapshot.AccessConfig.EnabledTypes = []string{"contour-http-proxy"}
	desired = (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.AccessHTTPProxies) != 1 {
		t.Fatalf("HTTPProxy was not derived: %#v", desired.AccessHTTPProxies)
	}
	host, _, _ := unstructured.NestedString(desired.AccessHTTPProxies[0].Object, "spec", "virtualhost", "fqdn")
	if host != "external-tls.site.apps.example" || !ownedBy(desired.AccessHTTPProxies[0].GetOwnerReferences(), secured.UID) {
		t.Fatalf("unexpected HTTPProxy: %#v", desired.AccessHTTPProxies[0])
	}
}

func TestSiteGeneratedAccessProjectsHTTPProxyEndpoints(t *testing.T) {
	snapshot := baseSnapshot()
	site := snapshot.Sites[0]
	site.Spec.LinkAccess = "contour-http-proxy"
	routerAccess := defaultRouterAccess(site, nil)
	routerAccess.UID = "router-access-uid"
	snapshot.RouterAccesses = []*skupperv2alpha1.RouterAccess{routerAccess}
	secured := desiredSecuredAccess("site", "skupper-router", "skupper-router", routerAccess, site.DefaultIssuer())
	secured.UID = "secured-uid"
	snapshot.SecuredAccesses = []*skupperv2alpha1.SecuredAccess{secured}
	controller := true
	snapshot.Services = []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: secured.Name, Namespace: "site", OwnerReferences: []metav1.OwnerReference{{UID: secured.UID, Controller: &controller}}}}}
	snapshot.AccessConfig = AccessConfig{EnabledTypes: []string{"contour-http-proxy"}, HTTPProxyDomain: "apps.example"}
	snapshot.HTTPProxies = securedAccessHTTPProxies(snapshot, secured)
	desired := (NamespaceDeriver{}).Derive(snapshot)
	if len(desired.Statuses.Sites) != 1 || len(desired.Statuses.Sites[0].Status.Endpoints) != 2 {
		t.Fatalf("HTTPProxy endpoints did not propagate to Site: %#v", desired.Statuses)
	}
	for _, endpoint := range desired.Statuses.Sites[0].Status.Endpoints {
		if endpoint.Group != "skupper-router" || endpoint.Host == "" || endpoint.Port != "443" {
			t.Fatalf("unexpected Site HTTPProxy endpoint: %#v", endpoint)
		}
	}
}

func TestTLSRouteDesiredSpecIncludesGatewayAPIDefaults(t *testing.T) {
	access := &skupperv2alpha1.SecuredAccess{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "site", UID: "access-uid"}, Spec: skupperv2alpha1.SecuredAccessSpec{Ports: []skupperv2alpha1.SecuredAccessPort{{Name: "tls", Port: 443}}}}
	snapshot := Snapshot{AccessConfig: AccessConfig{ControllerNamespace: "controller-ns"}}
	route := securedAccessTLSRoutes(snapshot, access, "apps.example")[0]
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	backends, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	parent := parents[0].(map[string]interface{})
	backend := backends[0].(map[string]interface{})["backendRefs"].([]interface{})[0].(map[string]interface{})
	if parent["group"] != "gateway.networking.k8s.io" || parent["kind"] != "Gateway" || backend["group"] != "" || backend["kind"] != "Service" || backend["weight"] != int64(1) {
		t.Fatalf("Gateway API defaults missing from TLSRoute: %#v", route.Object["spec"])
	}
}
