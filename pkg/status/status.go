// Package status contains the components that maintain the source of Skupper
// status within a site.
//
// Contains a StatusCollector that assembles status from known and remote state
// through the vanflow protocol to produce a Status, and StatusUpdateHandlers
// that handle persisting that status to the platform-appropriate location.
package status

import (
	"context"

	"github.com/c-kruse/vanflow/eventsource"
	"github.com/skupperproject/skupper/pkg/network"
)

type StatusCollector interface {
	Run(context.Context, func(status network.NetworkStatusInfo))
	HintEventSource(source eventsource.Info)
}

type StatusUpdateHandler interface {
	Start(context.Context)
	Handle(status network.NetworkStatusInfo)
}