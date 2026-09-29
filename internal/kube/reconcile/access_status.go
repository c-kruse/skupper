package reconcile

import (
	"reflect"
	"sort"
	"strconv"
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
	services := map[string]*corev1.Service{}
	for _, service := range snapshot.Services {
		services[service.Name] = service
	}
	routes := map[string]string{}
	for _, route := range snapshot.Routes {
		for _, ingress := range route.Status.Ingress {
			if ingress.Host != "" {
				routes[route.Name] = ingress.Host
				break
			}
		}
	}
	accesses := append([]*skupperv2alpha1.SecuredAccess(nil), snapshot.SecuredAccesses...)
	sort.Slice(accesses, func(i, j int) bool { return accesses[i].Name < accesses[j].Name })
	for _, current := range accesses {
		updated := current.DeepCopy()
		before := updated.Status.DeepCopy()
		configured := pendingState("Exposure Service has not been realized")
		resolved := pendingState("No external endpoint has been resolved")
		var endpoints []skupperv2alpha1.Endpoint
		if service := services[updated.Name]; service != nil && ownedBy(service.OwnerReferences, updated.UID) {
			configured = skupperv2alpha1.ReadyCondition()
			accessType := updated.Spec.AccessType
			if accessType == "" {
				accessType = snapshot.DefaultAccessType
			}
			switch accessType {
			case "local":
				for _, port := range updated.Spec.Ports {
					endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: updated.Name + "." + updated.Namespace, Port: strconv.Itoa(port.Port)})
				}
			case "loadbalancer":
				for _, ingress := range service.Status.LoadBalancer.Ingress {
					host := ingress.IP
					if host == "" {
						host = ingress.Hostname
					}
					for _, port := range service.Spec.Ports {
						if host != "" {
							endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: host, Port: strconv.Itoa(int(port.Port))})
						}
					}
				}
			case "route":
				for _, port := range updated.Spec.Ports {
					if host := routes[updated.Name+"-"+port.Name]; host != "" {
						endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: host, Port: "443"})
					}
				}
			case "nodeport":
				if snapshot.ClusterHost == "" {
					resolved = unknownState("Cluster host is not configured for nodeport access")
				} else {
					for _, port := range service.Spec.Ports {
						if port.NodePort != 0 {
							endpoints = append(endpoints, skupperv2alpha1.Endpoint{Name: port.Name, Host: snapshot.ClusterHost, Port: strconv.Itoa(int(port.NodePort))})
						}
					}
				}
			default:
				resolved = unknownState("Endpoint observation for this access type is unavailable")
			}
			if len(endpoints) > 0 {
				resolved = skupperv2alpha1.ReadyCondition()
			}
		}
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
