package reconcile

import (
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func copyInputs(in NamespaceInputs) NamespaceInputs {
	out := NamespaceInputs{}
	for _, value := range in.Sites {
		out.Sites = append(out.Sites, value.DeepCopy())
	}
	for _, value := range in.Listeners {
		out.Listeners = append(out.Listeners, value.DeepCopy())
	}
	for _, value := range in.MultiKeyListeners {
		out.MultiKeyListeners = append(out.MultiKeyListeners, value.DeepCopy())
	}
	for _, value := range in.Links {
		out.Links = append(out.Links, value.DeepCopy())
	}
	for _, value := range in.RouterAccesses {
		out.RouterAccesses = append(out.RouterAccesses, value.DeepCopy())
	}
	for _, value := range in.Certificates {
		out.Certificates = append(out.Certificates, value.DeepCopy())
	}
	for _, value := range in.SecuredAccesses {
		out.SecuredAccesses = append(out.SecuredAccesses, value.DeepCopy())
	}
	for _, value := range in.Services {
		out.Services = append(out.Services, value.DeepCopy())
	}
	for _, value := range in.Secrets {
		out.Secrets = append(out.Secrets, value.DeepCopy())
	}
	return out
}

func copyAllocations(in AllocationState) AllocationState {
	out := AllocationState{SiteUID: in.SiteUID, Ports: map[string]int{}}
	for key, value := range in.Ports {
		out.Ports[key] = value
	}
	return out
}

func copyObservations(in map[RouterTarget]Observation) map[RouterTarget]Observation {
	out := make(map[RouterTarget]Observation, len(in))
	for target, observation := range in {
		copy := observation
		copy.Resources = make(map[routercontrol.ResourceID]routercontrol.LocalResourceObservation, len(observation.Resources))
		for key, value := range observation.Resources {
			copy.Resources[key] = value
		}
		out[target] = copy
	}
	return out
}
