package reconcile

import (
	"github.com/skupperproject/skupper/internal/routercontrol"
)

func copyAllocations(in AllocationState) AllocationState {
	out := AllocationState{SiteUID: in.SiteUID, Ports: map[string]int{}, ResourceVersion: in.ResourceVersion}
	for key, value := range in.Ports {
		out.Ports[key] = value
	}
	return out
}

func copyObservations(in map[RouterTarget][]Observation) map[RouterTarget][]Observation {
	out := make(map[RouterTarget][]Observation, len(in))
	for target, observations := range in {
		for _, observation := range observations {
			copy := observation
			if observation.Application != nil {
				copy.Application = new(routercontrol.ApplicationReport)
				*copy.Application = *observation.Application
				copy.Application.Resources = append([]routercontrol.ResourceApplication(nil), observation.Application.Resources...)
				copy.Application.Credentials = append([]routercontrol.CredentialRevision(nil), observation.Application.Credentials...)
			}
			copy.Scopes = make(map[string]ObservationScope, len(observation.Scopes))
			for key, value := range observation.Scopes {
				value.Snapshot.Resources = append([]routercontrol.LocalResourceObservation(nil), value.Snapshot.Resources...)
				value.Snapshot.Addresses = append([]routercontrol.LocalAddressObservation(nil), value.Snapshot.Addresses...)
				copy.Scopes[key] = value
			}
			out[target] = append(out[target], copy)
		}
	}
	return out
}
