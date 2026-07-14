package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/go-amqp"
	"gotest.tools/v3/assert"
)

func TestContainerPing(t *testing.T) {
	testCases := []struct {
		Name    string
		Factory func(t *testing.T) ContainerFactory
	}{
		{
			Name: "mock",
			Factory: func(t *testing.T) ContainerFactory {
				return NewMockContainerFactory()
			},
		}, {
			Name: "qdr",
			Factory: func(t *testing.T) ContainerFactory {
				return containersFromEnv(t)
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			factory := tc.Factory(t)
			cs := factory.Create()
			cr := factory.Create()

			channel := "ex/" + randomID()
			ctx := context.Background()
			cs.Start(ctx)
			cr.Start(ctx)
			s := cs.NewSender(channel, SenderOptions{})
			r := cr.NewReceiver(channel, ReceiverOptions{})

			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := 0; i < 64; i++ {
					func() {
						sctx, cancel := context.WithTimeout(ctx, time.Second)
						defer cancel()
						err := s.Send(sctx, amqp.NewMessage(
							[]byte(fmt.Sprintf("ping-%d", i))),
						)
						if err != nil {
							t.Error(err)
						}
					}()
				}
			}()

			for i := 0; i < 64; i++ {
				msg, err := r.Next(ctx)
				if err != nil {
					t.Error(err)
				}
				if err := r.Accept(ctx, msg); err != nil {
					t.Error(err)
				}
			}
			select {
			case <-time.After(time.Second * 2):
				t.Fatal("send timed out")
			case <-done: // pass
			}

			assert.Check(t, s.Close(ctx))
			err := s.Send(ctx, amqp.NewMessage([]byte("closed")))
			assert.ErrorContains(t, err, "closed")
		})
	}

}

func publishSession(c *container, sess *amqp.Session) *sessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.state
	c.state = &sessionState{sess: sess, done: make(chan struct{})}
	close(prev.done)
	return c.state
}

func containerWoken(state *sessionState) bool {
	select {
	case <-state.done:
		return true
	default:
		return false
	}
}

func testContainer(t *testing.T) *container {
	t.Helper()
	c, ok := NewContainer("amqp://test.invalid", ContainerConfig{}).(*container)
	if !ok {
		t.Fatal("expected NewContainer to return a *container")
	}
	return c
}

func TestContainerInvalidate(t *testing.T) {
	c := testContainer(t)
	sess := &amqp.Session{}
	state := publishSession(c, sess)

	if got := c.current(); got != state || got.sess != sess {
		t.Fatalf("expected the published session state: got %v", got)
	}
	if containerWoken(state) {
		t.Error("expected a freshly published session to be valid")
	}

	boom := errors.New("boom")
	c.invalidate(state, boom)

	got := c.current()
	if got.sess != nil {
		t.Error("expected the invalidated session to be withheld from links")
	}
	if got.err != boom {
		t.Errorf("expected the error to be retained until a session is published: got %v", got.err)
	}
	if !containerWoken(state) {
		t.Error("expected invalidation to wake the container to reconnect")
	}

	// a second link failing on the same session must not double close
	c.invalidate(state, boom)
}

func TestContainerInvalidateIgnoresStaleState(t *testing.T) {
	c := testContainer(t)
	boom := errors.New("boom")

	failed := publishSession(c, &amqp.Session{})
	c.invalidate(failed, boom)

	// the container reconnects while a link is still holding the old state
	healthy := publishSession(c, &amqp.Session{})

	c.invalidate(failed, boom)

	if got := c.current(); got != healthy {
		t.Error("expected a late failure on an old session to leave the new one alone")
	}
	if containerWoken(healthy) {
		t.Error("expected a late failure on an old session to not trigger a second reconnect")
	}
}

func TestContainerInvalidateIsSingleWake(t *testing.T) {
	c := testContainer(t)
	state := publishSession(c, &amqp.Session{})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			c.invalidate(state, fmt.Errorf("link %d failed", i))
		})
	}
	wg.Wait()

	if !containerWoken(state) {
		t.Error("expected the container to be woken")
	}
	if got := c.current(); got.sess != nil {
		t.Error("expected the session to be invalidated")
	}
}

func TestLinkSessionWakesOnReconnect(t *testing.T) {
	c := testContainer(t)
	publishSession(c, &amqp.Session{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for range 500 {
		c.invalidate(c.current(), errors.New("boom"))

		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
				if _, err := l.session(ctx); err != nil {
					t.Errorf("link never saw the replacement session: %v", err)
				}
			})
		}
		runtime.Gosched() // give the links a chance to read the invalidated session
		publishSession(c, &amqp.Session{})
		wg.Wait()
	}
}

func TestLinkSessionWaitsForReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testContainer(t)
		failed := publishSession(c, &amqp.Session{})
		l := c.newLink("test", ReceiverOptions{}, SenderOptions{})

		state, err := l.session(t.Context())
		if err != nil || state != failed {
			t.Fatalf("expected a healthy session to be returned immediately: got %v: %v", state, err)
		}

		// a sibling link fails, invalidating the session out from under this one
		c.invalidate(failed, errors.New("boom"))

		type result struct {
			state *sessionState
			err   error
		}
		results := make(chan result, 1)
		go func() {
			state, err := l.session(t.Context())
			results <- result{state, err}
		}()

		synctest.Wait() // the link is now durably blocked awaiting a session
		select {
		case got := <-results:
			t.Fatalf("expected session to block while the container reconnects: got %v: %v", got.state, got.err)
		default:
		}

		healthy := publishSession(c, &amqp.Session{})
		got := <-results
		if got.err != nil {
			t.Fatalf("expected the replacement session: %v", got.err)
		}
		if got.state != healthy {
			t.Errorf("expected the link to adopt the replacement session: got %v", got.state)
		}
	})
}

func TestLinkSessionAdoptsNewSessionWithoutFailing(t *testing.T) {
	c := testContainer(t)
	l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
	ctx := t.Context()

	publishSession(c, &amqp.Session{})

	// the container reconnects without this link ever failing an operation
	healthy := publishSession(c, &amqp.Session{})

	state, err := l.session(ctx)
	if err != nil {
		t.Fatalf("expected a session: %v", err)
	}
	if state != healthy {
		t.Errorf("expected an idle link to adopt the current session: got %v", state)
	}
}

func TestLinkSessionClosed(t *testing.T) {
	c := testContainer(t)
	l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
	publishSession(c, &amqp.Session{})

	if err := l.Close(t.Context()); err != nil {
		t.Fatalf("expected close to succeed: %v", err)
	}
	if _, err := l.session(t.Context()); !errors.Is(err, errLinkClosed) {
		t.Errorf("expected a closed link to stop awaiting sessions: got %v", err)
	}
	if err := l.Accept(t.Context(), nil); !errors.Is(err, errLinkClosed) {
		t.Errorf("expected a closed link to reject accepts: got %v", err)
	}
}

func TestLinkAcceptStaleDelivery(t *testing.T) {
	c := testContainer(t)
	l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
	publishSession(c, &amqp.Session{})

	// no receiver is attached: the session that delivered the message is gone
	if err := l.Accept(t.Context(), nil); !errors.Is(err, errStaleDelivery) {
		t.Errorf("expected accept on a replaced session to report a stale delivery: got %v", err)
	}
}

func TestLinkHandleErrorDetachesStaleAttachments(t *testing.T) {
	c := testContainer(t)
	l := c.newLink("test", ReceiverOptions{}, SenderOptions{})

	stale := publishSession(c, &amqp.Session{})
	l.mu.Lock()
	l.rcv, l.rcvState = &amqp.Receiver{}, stale
	l.mu.Unlock()
	c.invalidate(stale, errors.New("boom"))

	// the link fails to attach to the replacement session
	next := publishSession(c, &amqp.Session{})
	l.handleError(t.Context(), next, errors.New("receiver create error"))

	l.mu.Lock()
	rcv := l.rcv
	l.mu.Unlock()
	if rcv != nil {
		t.Error("expected handleError to detach the receiver cached from an older session")
	}
	if err := l.Accept(t.Context(), nil); !errors.Is(err, errStaleDelivery) {
		t.Errorf("expected accept after the failure to report a stale delivery: got %v", err)
	}
}

func containersFromEnv(t *testing.T) ContainerFactory {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping test that requires external router")
	}
	var factory ContainerFactory
	err := func() error {
		qdr := os.Getenv("SKUPPER_ROUTER_AMQP_ADDRESS")
		if qdr == "" {
			return fmt.Errorf("SKUPPER_ROUTER_AMQP_ADDRESS environment variable not present")
		}
		setupCtx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()
		conn, err := amqp.Dial(setupCtx, qdr, nil)
		if err != nil {
			return fmt.Errorf("could not establish connection to router: %v", err)
		}
		conn.Close()
		factory = NewContainerFactory(qdr, ContainerConfig{
			ContainerID: "tc/" + randomID(),
		})
		return nil
	}()
	if err != nil {
		t.Skipf("skipping test that requires external router: %s", err)
	}

	return factory
}
