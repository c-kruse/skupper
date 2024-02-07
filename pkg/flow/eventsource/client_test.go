package eventsource

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/interconnectedcloud/go-amqp"
	v1 "github.com/skupperproject/skupper/pkg/flow/v1"
	"github.com/skupperproject/skupper/pkg/messaging"
	"gotest.tools/assert"
)

func TestClient(t *testing.T) {
	t.Parallel()
	tstCtx, tstCancel := context.WithCancel(context.Background())
	defer tstCancel()
	factory := messaging.NewMockConnectionFactory(t, "mockamqp://local")

	client := NewClient(factory, Info{
		ID:      "test",
		Address: "mc/sfe.test",
	})
	heartbeats := make(chan v1.HeartbeatMessage, 8)
	records := make(chan v1.RecordMessage, 8)
	client.OnHeartbeat(func(m v1.HeartbeatMessage) { heartbeats <- m })
	client.OnRecord(func(m v1.RecordMessage) { records <- m })

	tstConn, _ := factory.Connect()
	sender, _ := tstConn.Sender("mc/sfe.test")
	assert.Check(t, client.Listen(tstCtx, FromSourceAddress()))
	factory.Broker.AwaitReceivers("mc/sfe.test", 1)

	heartbeat := v1.HeartbeatMessage{
		Identity: "test", Version: 1, Now: 22,
		Source: "mc/sfe.test",
	}
	for i := 0; i < 10; i++ {
		sender.Send(heartbeat.Encode())
		actual := <-heartbeats
		assert.DeepEqual(t, actual, heartbeat)
		heartbeat.Now++
	}
	record := v1.RecordMessage{
		Address: "mc/sfe.test",
	}
	for i := 0; i < 10; i++ {
		msg, err := record.Encode()
		assert.Check(t, err)
		sender.Send(msg)
		actual := <-records
		assert.DeepEqual(t, actual, record)
		name := fmt.Sprintf("router-%d", i)
		record.Records = append(record.Records, &v1.RouterRecord{BaseRecord: v1.BaseRecord{Identity: name}})
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		client.Close()
	}()
	select {
	case <-closed: //okay
	case <-time.After(500 * time.Millisecond):
		t.Error("expected client.Close() to promptly return")
	}

	msg, err := record.Encode()
	assert.Check(t, err)
	sender.Send(msg)
	select {
	case <-time.After(100 * time.Millisecond): //okay
	case <-records:
		t.Error("expected client to stop handling records after close called")
	}

}

func TestClientFlush(t *testing.T) {
	t.Parallel()
	tstCtx, tstCancel := context.WithCancel(context.Background())
	defer tstCancel()
	factory := messaging.NewMockConnectionFactory(t, "mockamqp://local")

	client := NewClient(factory, Info{
		ID:      "test",
		Address: "mc/sfe.test",
		Direct:  "sfe.test",
	})
	tstConn, _ := factory.Connect()
	receiver, _ := tstConn.Receiver("sfe.test", 20)

	flush := make(chan *amqp.Message, 1)
	go func() {
		msg, err := receiver.Receive()
		assert.Check(t, err)
		flush <- msg
	}()

	assert.Check(t, client.SendFlush(tstCtx))

	select {
	case <-time.After(time.Millisecond * 500):
		t.Errorf("expected flush message")
	case msg := <-flush:
		assert.Equal(t, msg.Properties.Subject, "FLUSH")
	}

}
