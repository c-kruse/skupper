package adaptor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/routercontrol"
)

type blockingEventReceiver struct {
	ctx   context.Context
	calls atomic.Int32
}

func (r *blockingEventReceiver) NextEvent() (routercontrol.ServerEvent, error) {
	if r.calls.Add(1) <= 2 {
		return routercontrol.ServerEvent{Refresh: &routercontrol.RefreshRequest{RequestID: "refresh", Scope: routercontrol.ObservationScopeResources}}, nil
	}
	<-r.ctx.Done()
	return routercontrol.ServerEvent{}, r.ctx.Err()
}

func TestCertificateRenewalDelayUsesRemainingLifetime(t *testing.T) {
	now := time.Unix(1_000, 0)
	tests := []struct {
		name      string
		remaining time.Duration
		jitter    float64
		want      time.Duration
	}{
		{name: "default fifteen minute certificate", remaining: 15 * time.Minute, want: 10 * time.Minute},
		{name: "short token limited certificate", remaining: 90 * time.Second, want: 60 * time.Second},
		{name: "long certificate bounded margin", remaining: 24 * time.Hour, want: 24*time.Hour - 15*time.Minute},
		{name: "negative jitter", remaining: 15 * time.Minute, jitter: -1, want: 10*time.Minute + 30*time.Second},
		{name: "positive jitter", remaining: 15 * time.Minute, jitter: 1, want: 9*time.Minute + 30*time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := certificateRenewalDelay(now, now.Add(test.remaining), test.jitter); got != test.want {
				t.Fatalf("renewal delay %s, want %s", got, test.want)
			}
		})
	}
	if got := certificateRenewalDelay(now, now.Add(-time.Second), 0); got != 0 {
		t.Fatalf("expired certificate delay %s, want immediate", got)
	}
}

func TestBoundedCallCancelsAndJoinsStalledOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now()
	err := boundedCall(ctx, cancel, 10*time.Millisecond, func() error {
		defer close(done)
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("stalled operation was not canceled: err=%v context=%v", err, ctx.Err())
	}
	select {
	case <-done:
	default:
		t.Fatal("bounded call returned before operation goroutine exited")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled operation was not bounded: %s", elapsed)
	}
}

func TestControlReceiverCancellationJoinsWhenEventBufferIsAbandoned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	receiver := &blockingEventReceiver{ctx: ctx}
	_, done := startControlReceiver(ctx, receiver)
	deadline := time.After(time.Second)
	for receiver.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("receiver did not reach blocked event delivery")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receiver goroutine did not exit after session cancellation")
	}
}
