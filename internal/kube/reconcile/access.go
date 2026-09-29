package reconcile

import (
	"fmt"
	"sort"
	"strings"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

const controlledAnnotation = "internal.skupper.io/controlled"

func deriveAccessComposition(snapshot Snapshot, desired *DesiredNamespace, site *skupperv2alpha1.Site, groups []string) []*skupperv2alpha1.RouterAccess {
	accesses := sortedRouterAccess(snapshot.RouterAccesses)
	effective := make([]*skupperv2alpha1.RouterAccess, 0, len(accesses)+1)
	var currentDefault *skupperv2alpha1.RouterAccess
	for _, access := range accesses {
		if access.Name == "skupper-router" && access.Annotations[controlledAnnotation] == "true" && ownedBy(access.OwnerReferences, site.UID) {
			currentDefault = access
			continue
		}
		effective = append(effective, access)
	}
	if site.Spec.LinkAccess != "" && site.Spec.LinkAccess != "none" {
		generated := defaultRouterAccess(site, currentDefault)
		desired.GeneratedAccess = generated.DeepCopy()
		effective = append(effective, generated)
	}
	sort.Slice(effective, func(i, j int) bool { return effective[i].Name < effective[j].Name })

	currentSecured := map[string]*skupperv2alpha1.SecuredAccess{}
	for _, access := range snapshot.SecuredAccesses {
		currentSecured[access.Name] = access
	}
	for _, access := range effective {
		if access.UID == "" {
			continue
		}
		for index, group := range groups {
			name := access.Name
			if index > 0 {
				name = fmt.Sprintf("%s-%d", access.Name, index+1)
			}
			secured := desiredSecuredAccess(site.Namespace, name, group, access, site.DefaultIssuer())
			if current := currentSecured[name]; current != nil && ownedBy(current.OwnerReferences, access.UID) {
				secured.UID = current.UID
				secured.ResourceVersion = current.ResourceVersion
				secured.Generation = current.Generation
				secured.Status = *current.Status.DeepCopy()
			}
			desired.SecuredAccesses = append(desired.SecuredAccesses, secured)
		}
	}

	allSecured := map[string]*skupperv2alpha1.SecuredAccess{}
	for _, access := range snapshot.SecuredAccesses {
		allSecured[access.Name] = access
	}
	for _, access := range desired.SecuredAccesses {
		if access.UID != "" {
			allSecured[access.Name] = access
		}
	}
	for _, name := range sortedKeys(allSecured) {
		access := allSecured[name]
		desired.AccessServices = append(desired.AccessServices, securedAccessService(access, snapshot.DefaultAccessType))
		accessType := access.Spec.AccessType
		if accessType == "" {
			accessType = snapshot.DefaultAccessType
		}
		if accessType == "route" {
			for _, port := range access.Spec.Ports {
				desired.AccessRoutes = append(desired.AccessRoutes, securedAccessRoute(access, port))
			}
		}
	}
	deriveCertificates(snapshot, desired, site, allSecured)
	return effective
}

func securedAccessRoute(access *skupperv2alpha1.SecuredAccess, port skupperv2alpha1.SecuredAccessPort) *routev1.Route {
	controller, block, weight := true, true, int32(100)
	name := access.Name + "-" + port.Name
	host := access.Spec.Settings["domain"]
	if host != "" {
		host = name + "." + access.Namespace + "." + host
	}
	return &routev1.Route{TypeMeta: metav1.TypeMeta{APIVersion: routev1.GroupVersion.String(), Kind: "Route"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: access.Namespace, Labels: map[string]string{"internal.skupper.io/secured-access": "true"}, Annotations: map[string]string{controlledAnnotation: "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "SecuredAccess", Name: access.Name, UID: access.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Spec: routev1.RouteSpec{Host: host, Port: &routev1.RoutePort{TargetPort: intstr.FromString(port.Name)}, To: routev1.RouteTargetReference{Kind: "Service", Name: access.Name, Weight: &weight}, TLS: &routev1.TLSConfig{Termination: routev1.TLSTerminationPassthrough, InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyNone}}}
}

func defaultRouterAccess(site *skupperv2alpha1.Site, current *skupperv2alpha1.RouterAccess) *skupperv2alpha1.RouterAccess {
	controller, block := true, true
	accessType := site.Spec.LinkAccess
	if accessType == "default" {
		accessType = ""
	}
	settings := map[string]string{}
	if current != nil {
		for key, value := range current.Spec.Settings {
			settings[key] = value
		}
	}
	if value, found := site.Spec.Settings["ingressClassName"]; found {
		if value = strings.TrimSpace(value); value == "" {
			delete(settings, "ingressClassName")
		} else {
			settings["ingressClassName"] = value
		}
	}
	if len(settings) == 0 {
		settings = nil
	}
	result := &skupperv2alpha1.RouterAccess{TypeMeta: metav1.TypeMeta{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "RouterAccess"}, ObjectMeta: metav1.ObjectMeta{Name: "skupper-router", Namespace: site.Namespace, Annotations: map[string]string{controlledAnnotation: "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: site.Name, UID: site.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Spec: skupperv2alpha1.RouterAccessSpec{AccessType: accessType, TlsCredentials: "skupper-site-server", Issuer: "skupper-site-ca", GenerateTlsCredentials: true, Roles: []skupperv2alpha1.RouterAccessRole{{Name: "inter-router", Port: 55671}, {Name: "edge", Port: 45671}}, Settings: settings}}
	if current != nil {
		result.UID, result.ResourceVersion, result.Generation, result.Status = current.UID, current.ResourceVersion, current.Generation, *current.Status.DeepCopy()
	}
	return result
}

func desiredSecuredAccess(namespace, name, group string, access *skupperv2alpha1.RouterAccess, defaultIssuer string) *skupperv2alpha1.SecuredAccess {
	controller, block := true, true
	issuer := ""
	if access.Spec.GenerateTlsCredentials {
		issuer = access.Spec.Issuer
		if issuer == "" {
			issuer = defaultIssuer
		}
	}
	result := &skupperv2alpha1.SecuredAccess{TypeMeta: metav1.TypeMeta{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "SecuredAccess"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: map[string]string{controlledAnnotation: "true", "internal.skupper.io/routeraccess": access.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "RouterAccess", Name: access.Name, UID: access.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Spec: skupperv2alpha1.SecuredAccessSpec{AccessType: access.Spec.AccessType, Selector: map[string]string{"skupper.io/component": "router", "skupper.io/group": group}, Certificate: access.Spec.TlsCredentials, Issuer: issuer, Settings: copyStrings(access.Spec.Settings)}}
	for _, role := range access.Spec.Roles {
		result.Spec.Ports = append(result.Spec.Ports, skupperv2alpha1.SecuredAccessPort{Name: role.Name, Port: int(role.GetPort()), TargetPort: int(role.GetPort()), Protocol: string(corev1.ProtocolTCP)})
	}
	return result
}

func securedAccessService(access *skupperv2alpha1.SecuredAccess, defaultAccessType string) *corev1.Service {
	accessType := access.Spec.AccessType
	if accessType == "" {
		accessType = defaultAccessType
	}
	serviceType := corev1.ServiceTypeClusterIP
	if accessType == "loadbalancer" {
		serviceType = corev1.ServiceTypeLoadBalancer
	} else if accessType == "nodeport" {
		serviceType = corev1.ServiceTypeNodePort
	}
	controller, block := true, true
	policy := corev1.ServiceInternalTrafficPolicyCluster
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: access.Name, Namespace: access.Namespace, Labels: map[string]string{"internal.skupper.io/secured-access": "true"}, Annotations: map[string]string{controlledAnnotation: "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "SecuredAccess", Name: access.Name, UID: access.UID, Controller: &controller, BlockOwnerDeletion: &block}}}, Spec: corev1.ServiceSpec{Type: serviceType, SessionAffinity: corev1.ServiceAffinityNone, InternalTrafficPolicy: &policy, Selector: copyStrings(access.Spec.Selector)}}
	for _, port := range access.Spec.Ports {
		service.Spec.Ports = append(service.Spec.Ports, corev1.ServicePort{Name: port.Name, Port: int32(port.Port), TargetPort: intstr.FromInt(port.TargetPort), Protocol: corev1.Protocol(port.Protocol)})
	}
	return service
}

func deriveCertificates(snapshot Snapshot, desired *DesiredNamespace, site *skupperv2alpha1.Site, accesses map[string]*skupperv2alpha1.SecuredAccess) {
	siteOwner := []metav1.OwnerReference{{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Site", Name: site.Name, UID: site.UID}}
	desired.Certificates = append(desired.Certificates,
		desiredCertificate(site.Namespace, "skupper-site-ca", skupperv2alpha1.CertificateSpec{Subject: site.Name + " site CA", Signing: true}, siteOwner, nil),
		desiredCertificate(site.Namespace, "skupper-local-ca", skupperv2alpha1.CertificateSpec{Subject: site.Name + " local CA", Signing: true}, siteOwner, nil),
		desiredCertificate(site.Namespace, "skupper-local-server", skupperv2alpha1.CertificateSpec{Ca: "skupper-local-ca", Subject: "skupper-router-local", Hosts: []string{"skupper-router-local", "skupper-router-local." + site.Namespace, "skupper-router-local." + site.Namespace + ".svc.cluster.local"}, Server: true}, siteOwner, map[string][]string{string(site.UID): {"skupper-router-local", "skupper-router-local." + site.Namespace, "skupper-router-local." + site.Namespace + ".svc.cluster.local"}}),
	)
	byCertificate := map[string][]*skupperv2alpha1.SecuredAccess{}
	for _, access := range accesses {
		if access.UID == "" || access.Spec.Issuer == "" {
			continue
		}
		name := access.Spec.Certificate
		if name == "" {
			name = access.Name
		}
		byCertificate[name] = append(byCertificate[name], access)
	}
	current := map[string]*skupperv2alpha1.Certificate{}
	for _, certificate := range snapshot.Certificates {
		current[certificate.Name] = certificate
	}
	for _, name := range sortedKeys(byCertificate) {
		owners := []metav1.OwnerReference{}
		hostsByOwner := map[string][]string{}
		accessList := byCertificate[name]
		sort.Slice(accessList, func(i, j int) bool { return accessList[i].Name < accessList[j].Name })
		for _, access := range accessList {
			owners = append(owners, metav1.OwnerReference{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "SecuredAccess", Name: access.Name, UID: access.UID})
			hosts := []string{access.Name, access.Name + "." + access.Namespace}
			for _, endpoint := range access.Status.Endpoints {
				if endpoint.Host != "" {
					hosts = append(hosts, endpoint.Host)
				}
			}
			hostsByOwner[string(access.UID)] = uniqueSorted(hosts)
		}
		subject := accessList[0].Name
		if existing := current[name]; existing != nil && len(existing.OwnerReferences) > 1 {
			subject = existing.Spec.Subject
		}
		desired.Certificates = append(desired.Certificates, desiredCertificate(site.Namespace, name, skupperv2alpha1.CertificateSpec{Ca: accessList[0].Spec.Issuer, Subject: subject, Server: true}, owners, hostsByOwner))
	}
	sort.Slice(desired.Certificates, func(i, j int) bool { return desired.Certificates[i].Name < desired.Certificates[j].Name })
}

func desiredCertificate(namespace, name string, spec skupperv2alpha1.CertificateSpec, owners []metav1.OwnerReference, hostsByOwner map[string][]string) *skupperv2alpha1.Certificate {
	annotations := map[string]string{controlledAnnotation: "true"}
	for uid, hosts := range hostsByOwner {
		annotations["internal.skupper.io/hosts-"+uid] = strings.Join(hosts, ",")
		spec.Hosts = append(spec.Hosts, hosts...)
	}
	spec.Hosts = uniqueSorted(spec.Hosts)
	return &skupperv2alpha1.Certificate{TypeMeta: metav1.TypeMeta{APIVersion: skupperv2alpha1.SchemeGroupVersion.String(), Kind: "Certificate"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"internal.skupper.io/certificate": "true"}, Annotations: annotations, OwnerReferences: owners}, Spec: spec}
}

func ownedBy(owners []metav1.OwnerReference, uid types.UID) bool {
	for _, owner := range owners {
		if owner.UID == uid {
			return true
		}
	}
	return false
}

func copyStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func sortedKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if value != "" && (len(result) == 0 || result[len(result)-1] != value) {
			result = append(result, value)
		}
	}
	return result
}
