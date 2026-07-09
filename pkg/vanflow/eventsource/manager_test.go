package eventsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/Azure/go-amqp"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
	"gotest.tools/v3/poll"
)

func TestManagerClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping flaky test: #1738")
	}
	tstCtx, tstCancel := context.WithCancel(context.Background())
	defer tstCancel()

	sourceID := uniqueSuffix("test-event-source-manager")
	factory, rtt := requireContainers(t)
	ctr := factory.Create()
	ctr.Start(tstCtx)
	ctrClient := factory.Create()
	ctrClient.Start(tstCtx)

	discovery := NewDiscovery(ctrClient, DiscoveryOptions{})
	go discovery.Run(tstCtx, DiscoveryHandlers{})

	sourceRef := store.SourceRef{ID: sourceID}
	source := Info{ID: sourceID, Type: "test-event-source", Address: mcsfe(sourceID), Direct: sfe(sourceID)}
	logStor := store.NewSyncMapStore(store.SyncMapStoreConfig{})
	listenerStor := store.NewSyncMapStore(store.SyncMapStoreConfig{})
	for i := 0; i < 64; i++ {
		logStor.Add(vanflow.LogRecord{BaseRecord: vanflow.NewBase(fmt.Sprintf("log-%d", i))}, sourceRef)
		listenerStor.Add(vanflow.ListenerRecord{BaseRecord: vanflow.NewBase(fmt.Sprintf("listener-%d", i))}, sourceRef)
	}
	manager := NewManager(ctr, ManagerConfig{
		Source:            source,
		Stores:            []store.Interface{logStor, listenerStor},
		HeartbeatInterval: rtt * 10,
		BeaconInterval:    rtt * 50,
	})
	go manager.Run(tstCtx)

	clientStor := store.NewSyncMapStore(store.SyncMapStoreConfig{})
	logsOnly := RecordStoreRouter{
		Source: sourceRef,
		Stores: RecordStoreMap{vanflow.LogRecord{}.GetTypeMeta().String(): clientStor},
	}

	client := NewClient(ctrClient, ClientOptions{Source: source})
	client.OnRecord(logsOnly.Route)
	client.Listen(tstCtx, FromSourceAddress())
	defer client.Close()
	t.Run("wait to flush", func(t *testing.T) {
		flushCtx, cancel := context.WithTimeout(tstCtx, rtt*100)
		defer cancel()
		if err := FlushOnFirstMessage(flushCtx, client); err != nil {
			t.Errorf("expected manager to be discovered by the client: %s", err)
		}
	})

	t.Run("will synchronize records", func(t *testing.T) {
		poll.WaitOn(t, func(t poll.LogT) poll.Result {
			actual := clientStor.List()
			expected := logStor.List()
			if !cmp.Equal(actual, expected, ignoreLastUpdateAndOrder...) {
				return poll.Continue("waiting for all records to be synced: %s", cmp.Diff(actual, expected, ignoreLastUpdateAndOrder...))
			}
			return poll.Success()
		}, poll.WithDelay(rtt), poll.WithTimeout(300*rtt))
	})

	t.Run("can publish updates", func(t *testing.T) {
		prevEntry, _ := logStor.Get("log-1")
		updated := prevEntry.Record.(vanflow.LogRecord)
		txt := "updated txt"
		updated.LogText = &txt
		manager.PublishUpdate(RecordUpdate{Prev: prevEntry.Record, Curr: updated})
		poll.WaitOn(t, func(t poll.LogT) poll.Result {
			actual, _ := clientStor.Get("log-1")
			expected := updated
			if !cmp.Equal(actual.Record, expected) {
				return poll.Continue("waiting for record '1' to be updated: %s", cmp.Diff(actual.Record, expected))
			}
			return poll.Success()
		}, poll.WithDelay(rtt), poll.WithTimeout(100*rtt))
	})

}

type flakySender struct {
	failures int
	attempts int
}

func (s *flakySender) Send(ctx context.Context, _ *amqp.Message) error {
	s.attempts++
	if s.attempts <= s.failures {
		return errors.New("send error: session closed")
	}
	return nil
}

func (s *flakySender) Close(context.Context) error { return nil }

func TestManagerSendRecordMessageRetries(t *testing.T) {
	testcases := []struct {
		name            string
		retries         int
		failures        int
		expectErr       bool
		expectedAttempt int
		expectedElapsed time.Duration
	}{
		{name: "sent on first attempt", expectedAttempt: 1},
		{name: "sent after transient failure", failures: 1, expectedAttempt: 2,
			expectedElapsed: recordSendRetryInterval},
		{name: "sent on final attempt", failures: defaultRecordSendRetries, expectedAttempt: defaultRecordSendRetries + 1,
			expectedElapsed: recordSendRetryInterval*7 + recordSendRetryIntervalMax*2}, // 250ms + 500ms + 1s, then capped at 2s
		{name: "dropped when retries exhausted", failures: defaultRecordSendRetries + 1, expectErr: true, expectedAttempt: defaultRecordSendRetries + 1,
			expectedElapsed: recordSendRetryInterval*7 + recordSendRetryIntervalMax*2},
		{name: "retries can be disabled", retries: -1, failures: 1, expectErr: true, expectedAttempt: 1},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sender := &flakySender{failures: tc.failures}
				manager := NewManager(nil, ManagerConfig{RecordSendRetries: tc.retries})

				start := time.Now()
				err := manager.sendRecordMessage(t.Context(), sender, vanflow.HeartbeatMessage{}.Encode())
				elapsed := time.Since(start)

				if tc.expectErr && err == nil {
					t.Error("expected send to return an error")
				}
				if !tc.expectErr && err != nil {
					t.Errorf("expected send to succeed: got %s", err)
				}
				if sender.attempts != tc.expectedAttempt {
					t.Errorf("expected %d send attempts: got %d", tc.expectedAttempt, sender.attempts)
				}
				if elapsed != tc.expectedElapsed {
					t.Errorf("expected retries to back off for %s: got %s", tc.expectedElapsed, elapsed)
				}
			})
		})
	}
}

func TestManagerSendRecordMessageBackoffIsCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const retries = 8
		sender := &flakySender{failures: retries + 1}
		manager := NewManager(nil, ManagerConfig{RecordSendRetries: retries})

		start := time.Now()
		manager.sendRecordMessage(t.Context(), sender, vanflow.HeartbeatMessage{}.Encode())

		// 250ms + 500ms + 1s, then capped at 2s for the remaining 5 retries
		expected := recordSendRetryInterval*7 + recordSendRetryIntervalMax*5
		if elapsed := time.Since(start); elapsed != expected {
			t.Errorf("expected backoff to be capped at %s, totalling %s: got %s", recordSendRetryIntervalMax, expected, elapsed)
		}
	})
}

type blockingSender struct{}

func (blockingSender) Send(ctx context.Context, _ *amqp.Message) error {
	<-ctx.Done()
	return fmt.Errorf("send error: %w", ctx.Err())
}

func (blockingSender) Close(context.Context) error { return nil }

func TestSendWithTimeout(t *testing.T) {
	t.Run("timeout is reported as a send timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			err := sendWithTimeout(t.Context(), time.Minute, blockingSender{}, vanflow.HeartbeatMessage{}.Encode())
			if !errors.Is(err, errSendTimeoutExceeded) {
				t.Errorf("expected send timeout: got %v", err)
			}
			if elapsed := time.Since(start); elapsed != time.Minute {
				t.Errorf("expected the send to be given the full timeout: got %s", elapsed)
			}
		})
	})
	t.Run("cancellation is not reported as a send timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				// resumes once the send is durably blocked on the context
				synctest.Wait()
				cancel()
			}()

			err := sendWithTimeout(ctx, time.Minute, blockingSender{}, vanflow.HeartbeatMessage{}.Encode())
			if errors.Is(err, errSendTimeoutExceeded) {
				t.Error("expected cancellation to not be reported as a send timeout")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("expected context cancellation: got %v", err)
			}
		})
	})
	t.Run("deadline on the parent context is not reported as a send timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			err := sendWithTimeout(ctx, time.Minute, blockingSender{}, vanflow.HeartbeatMessage{}.Encode())
			if errors.Is(err, errSendTimeoutExceeded) {
				t.Error("expected parent deadline to not be reported as a send timeout")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("expected context deadline exceeded: got %v", err)
			}
		})
	})
	t.Run("send errors are returned as is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sender := &flakySender{failures: 1}
			err := sendWithTimeout(t.Context(), time.Minute, sender, vanflow.HeartbeatMessage{}.Encode())
			if err == nil || errors.Is(err, errSendTimeoutExceeded) {
				t.Errorf("expected the sender's error: got %v", err)
			}
		})
	})
}

func TestManagerSendRecordMessageCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		sender := &flakySender{failures: 1}
		manager := NewManager(nil, ManagerConfig{})

		cancel()
		err := manager.sendRecordMessage(ctx, sender, vanflow.HeartbeatMessage{}.Encode())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context cancellation to stop retries: got %v", err)
		}
		if sender.attempts != 1 {
			t.Errorf("expected a single send attempt: got %d", sender.attempts)
		}
	})
}

var ignoreLastUpdateAndOrder = []cmp.Option{
	cmpopts.IgnoreFields(store.Metadata{}, "LastUpdate"),
	cmpopts.SortSlices(func(a, b store.Entry) bool {
		return strings.Compare(a.Record.Identity(), b.Record.Identity()) < 0
	}),
}
