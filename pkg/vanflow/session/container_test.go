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

// publishSession mimics the container's run loop publishing a newly dialed
// session, so that invalidation can be exercised without a live router.
func publishSession(c *container, sess *amqp.Session) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	close(c.hasNext)
	c.gen++
	c.sess, c.sessErr = sess, nil
	c.hasNext, c.invalidated = make(chan struct{}), make(chan struct{})
	return c.gen
}

func containerWoken(c *container) bool {
	c.mu.Lock()
	invalidated := c.invalidated
	c.mu.Unlock()
	select {
	case <-invalidated:
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
	gen := publishSession(c, sess)

	if got, gotGen, _ := c.currentSession(); got != sess || gotGen != gen {
		t.Fatalf("expected the published session at generation %d: got %v at %d", gen, got, gotGen)
	}
	if containerWoken(c) {
		t.Error("expected a freshly published session to be valid")
	}

	boom := errors.New("boom")
	c.invalidate(gen, boom)

	got, gotGen, _ := c.currentSession()
	if got != nil {
		t.Error("expected the invalidated session to be withheld from links")
	}
	if gotGen != gen {
		t.Errorf("expected the generation to be retained until a session is published: got %d", gotGen)
	}
	if !containerWoken(c) {
		t.Error("expected invalidation to wake the container to reconnect")
	}

	// a second link failing on the same session must not double close
	c.invalidate(gen, boom)
}

func TestContainerInvalidateIgnoresStaleGeneration(t *testing.T) {
	c := testContainer(t)
	boom := errors.New("boom")

	failed := &amqp.Session{}
	staleGen := publishSession(c, failed)
	c.invalidate(staleGen, boom)

	// the container reconnects while a link is still holding the old generation
	healthy := &amqp.Session{}
	publishSession(c, healthy)

	c.invalidate(staleGen, boom)

	if got, _, _ := c.currentSession(); got != healthy {
		t.Error("expected a late failure on an old session to leave the new one alone")
	}
	if containerWoken(c) {
		t.Error("expected a late failure on an old session to not trigger a second reconnect")
	}
}

func TestContainerInvalidateIsSingleWake(t *testing.T) {
	c := testContainer(t)
	gen := publishSession(c, &amqp.Session{})

	// every link attached to the session fails at once
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			c.invalidate(gen, fmt.Errorf("link %d failed", i))
		})
	}
	wg.Wait()

	if !containerWoken(c) {
		t.Error("expected the container to be woken")
	}
	if got, _, _ := c.currentSession(); got != nil {
		t.Error("expected the session to be invalidated")
	}
}

// TestLinkSessionWakesOnReconnect races links awaiting a session against the
// container publishing one. A link that reads the invalidated session and the
// hasNext channel separately can observe the session before publication and the
// channel after it, and then wait for a wakeup that has already happened.
func TestLinkSessionWakesOnReconnect(t *testing.T) {
	c := testContainer(t)
	publishSession(c, &amqp.Session{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for range 500 {
		c.mu.Lock()
		gen := c.gen
		c.mu.Unlock()
		c.invalidate(gen, errors.New("boom"))

		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
				if _, _, err := l.session(ctx); err != nil {
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
		failed := &amqp.Session{}
		failedGen := publishSession(c, failed)
		l := c.newLink("test", ReceiverOptions{}, SenderOptions{})

		sess, gen, err := l.session(t.Context())
		if err != nil || sess != failed || gen != failedGen {
			t.Fatalf("expected a healthy session to be returned immediately: got %v at %d: %v", sess, gen, err)
		}

		// a sibling link fails, invalidating the session out from under this one
		c.invalidate(failedGen, errors.New("boom"))

		healthy := &amqp.Session{}
		type result struct {
			sess *amqp.Session
			gen  int
			err  error
		}
		results := make(chan result, 1)
		go func() {
			sess, gen, err := l.session(t.Context())
			results <- result{sess, gen, err}
		}()

		synctest.Wait() // the link is now durably blocked awaiting a session
		select {
		case got := <-results:
			t.Fatalf("expected session to block while the container reconnects: got %v at %d: %v", got.sess, got.gen, got.err)
		default:
		}

		healthyGen := publishSession(c, healthy)
		got := <-results
		if got.err != nil {
			t.Fatalf("expected the replacement session: %v", got.err)
		}
		if got.sess != healthy || got.gen != healthyGen {
			t.Errorf("expected the link to adopt the replacement session at generation %d: got %v at %d", healthyGen, got.sess, got.gen)
		}
	})
}

func TestLinkSessionAdoptsNewGenerationWithoutFailing(t *testing.T) {
	c := testContainer(t)
	l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
	ctx := t.Context()

	publishSession(c, &amqp.Session{})

	// the container reconnects without this link ever failing an operation
	healthy := &amqp.Session{}
	healthyGen := publishSession(c, healthy)

	sess, gen, err := l.session(ctx)
	if err != nil {
		t.Fatalf("expected a session: %v", err)
	}
	if sess != healthy || gen != healthyGen {
		t.Errorf("expected an idle link to adopt the current session at generation %d: got %v at %d", healthyGen, sess, gen)
	}
}

func TestLinkSessionClosed(t *testing.T) {
	c := testContainer(t)
	l := c.newLink("test", ReceiverOptions{}, SenderOptions{})
	publishSession(c, &amqp.Session{})

	if err := l.Close(t.Context()); err != nil {
		t.Fatalf("expected close to succeed: %v", err)
	}
	if _, _, err := l.session(t.Context()); !errors.Is(err, errLinkClosed) {
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
