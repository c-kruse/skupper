package adaptor

import (
	"time"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

// RuntimeMetrics describes bounded adaptor lifecycle and high-level local
// management phases. A phase may contain multiple AMQP management requests.
type RuntimeMetrics interface {
	ConnectionAttempt(outcome string, duration time.Duration)
	SetControlStreamUp(bool)
	IntentAccepted()
	RealizationFinished(state routercontrol.ApplicationState, duration time.Duration)
	SetApplicationState(state string)
	AcceptedToApplied(time.Duration)
	LocalManagementFinished(operation, outcome string, duration time.Duration)
}

type NoopRuntimeMetrics struct{}

func (NoopRuntimeMetrics) ConnectionAttempt(string, time.Duration)                           {}
func (NoopRuntimeMetrics) SetControlStreamUp(bool)                                           {}
func (NoopRuntimeMetrics) IntentAccepted()                                                   {}
func (NoopRuntimeMetrics) RealizationFinished(routercontrol.ApplicationState, time.Duration) {}
func (NoopRuntimeMetrics) SetApplicationState(string)                                        {}
func (NoopRuntimeMetrics) AcceptedToApplied(time.Duration)                                   {}
func (NoopRuntimeMetrics) LocalManagementFinished(string, string, time.Duration)             {}
