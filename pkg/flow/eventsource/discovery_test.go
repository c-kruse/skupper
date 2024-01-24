package eventsource

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "github.com/skupperproject/skupper/pkg/flow/v1"
	"github.com/skupperproject/skupper/pkg/messaging"
	"gotest.tools/assert"
	"gotest.tools/poll"
)

func TestDiscoveryBasic(t *testing.T) {
	t.Parallel()
	tstCtx, tstCancel := context.WithCancel(context.Background())
	defer tstCancel()
	factory := messaging.NewMockConnectionFactory(t, "mockamqp://local")

	discovery := NewDiscovery(factory)

	discoveredOut := make(chan Info, 8)
	forgottenOut := make(chan Info, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		discovery.Run(tstCtx, DiscoveryHandlers{
			Discovered: func(info Info) {
				discoveredOut <- info
			},
			Forgotten: func(info Info) {
				forgottenOut <- info
			},
		})
	}()

	tstConn, _ := factory.Connect()
	tstSender, _ := tstConn.Sender("mc/sfe.all")
	factory.Broker.AwaitReceivers("mc/sfe.all", 1)
	beaconA := fixtureBeaconFor("a", "ROUTER")
	beaconB := fixtureBeaconFor("b", "CONTROLLER")

	tstSender.Send(beaconA.Encode())
	tstSender.Send(beaconB.Encode())

	// wait for discovery.List to return two sources
	poll.WaitOn(t,
		func(t poll.LogT) poll.Result {
			actual, desired := len(discovery.List()), 2
			if actual == desired {
				return poll.Success()
			}
			return poll.Continue("number of event sources is %d, not %d", actual, desired)
		}, poll.WithTimeout(time.Second),
	)

	// expect two events
	eventA := <-discoveredOut
	eventB := <-discoveredOut
	if eventA.ID == "b" {
		eventA, eventB = eventB, eventA
	}

	sourceA, ok := discovery.Get("a")
	assert.Check(t, ok)
	sourceB, ok := discovery.Get("b")
	assert.Check(t, ok)

	assert.DeepEqual(t, sourceA, eventA)
	assert.DeepEqual(t, sourceB, eventB)

	tstSender.Send(beaconA.Encode())
	tstSender.Send(beaconA.Encode())
	tstSender.Send(beaconA.Encode())
	tstSender.Send(beaconA.Encode())
	tstSender.Send(beaconB.Encode())
	tstSender.Send(beaconA.Encode())

	// wait for LastSeen to update
	poll.WaitOn(t,
		func(t poll.LogT) poll.Result {
			presentA, ok := discovery.Get("a")
			if !ok {
				return poll.Error(fmt.Errorf("error getting source 'a'"))
			}
			presentB, ok := discovery.Get("b")
			if !ok {
				return poll.Error(fmt.Errorf("error getting source 'b'"))
			}
			prevA, currentA := eventA.LastSeen, presentA.LastSeen
			prevB, currentB := eventB.LastSeen, presentB.LastSeen
			if currentA.After(prevA) && currentB.After(prevB) {
				return poll.Success()
			}
			return poll.Continue("waiting for lastseen to advance")
		}, poll.WithTimeout(1*time.Second),
	)
	assert.Check(t, len(discoveredOut) == 0, "expected no new discovery events after subsequent beacons")
	assert.Check(t, len(forgottenOut) == 0, "expected no new forgotten events after subsequent beacons")

	assert.Check(t, !discovery.Forget("c"), "expected to ignore call to Forget for unknown id")
	assert.Check(t, len(forgottenOut) == 0, "expected no new events after invalid call to Forget")

	assert.Check(t, discovery.Forget("a"), "expected ok to forget event source 'a'")
	// wait for discovery.List to return only one source
	poll.WaitOn(t,
		func(t poll.LogT) poll.Result {
			actual, desired := len(discovery.List()), 1
			if actual == desired {
				return poll.Success()
			}
			return poll.Continue("number of event sources is %d, not %d", actual, desired)
		}, poll.WithTimeout(time.Second),
	)

	// expect one event
	eventDelete := <-forgottenOut
	assert.Check(t, eventDelete.ID == "a")
	assert.Check(t, len(discovery.List()) == 1)
	_, ok = discovery.Get("a")
	assert.Check(t, !ok, "expected Get on forgotten ID to return not ok")

	tstCancel()
	select {
	case <-time.After(time.Millisecond * 500):
		t.Error("expected discovery.Run to finish after cancelling context")
	case <-done: // okay
	}
}

func TestDiscoveryWatch(t *testing.T) {
	t.Parallel()
	tstCtx, tstCancel := context.WithCancel(context.Background())
	defer tstCancel()
	factory := messaging.NewMockConnectionFactory(t, "mockamqp://local")

	discovery := NewDiscovery(factory)

	discoveredOut := make(chan Info, 8)
	forgottenOut := make(chan Info, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		discovery.Run(tstCtx, DiscoveryHandlers{
			Discovered: func(info Info) {
				discoveredOut <- info
			},
			Forgotten: func(info Info) {
				forgottenOut <- info
			},
		})
	}()

	tstConn, _ := factory.Connect()
	beaconSender, _ := tstConn.Sender("mc/sfe.all")
	heartbeatSender, _ := tstConn.Sender("mc/sfe.a")
	// continually send heartbeats for source a
	go func() {
		heartbeat := v1.HeartbeatMessage{
			Version:  1,
			Now:      1000,
			Identity: "a",
			Source:   "mc/sfe.a",
		}
		for {
			time.Sleep(time.Millisecond * 25)
			heartbeatSender.Send(heartbeat.Encode())
			heartbeat.Now++
		}
	}()

	// send a beacon for router a and await the discovery event
	factory.Broker.AwaitReceivers("mc/sfe.all", 1)
	beaconA := fixtureBeaconFor("a", "ROUTER")

	beaconSender.Send(beaconA.Encode())
	event := <-discoveredOut

	// start a new watched client and begin listening
	client, err := discovery.NewWatchClient(tstCtx, WatchConfig{ID: event.ID, Timeout: time.Millisecond * 250, GracePeriod: time.Second})
	assert.Check(t, err)

	listenCtx, listenCancel := context.WithCancel(tstCtx)
	client.Listen(listenCtx, FromSourceAddress())

	poll.WaitOn(t,
		func(t poll.LogT) poll.Result {
			present, ok := discovery.Get("a")
			if !ok {
				return poll.Error(fmt.Errorf("event source 'a' forgotten"))
			}
			prev, current := event.LastSeen, present.LastSeen
			if current.After(prev) {
				return poll.Success()
			}
			return poll.Continue("waiting for lastseen to advance")
		}, poll.WithTimeout(5*time.Second),
	)

	listenCancel()

	select {
	case event := <-forgottenOut:
		assert.Check(t, event.ID == "a")
	case <-time.After(time.Second):
		t.Error("expected source to be forgotten after starting watch client with no activity")
	}
}

func fixtureBeaconFor(id string, source string) v1.BeaconMessage {
	return v1.BeaconMessage{
		Version:    1,
		SourceType: source,
		Address:    fmt.Sprintf("mc/sfe.%s", id),
		Direct:     fmt.Sprintf("sfe.%s", id),
		Identity:   id,
	}
}
