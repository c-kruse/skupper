package leadership

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type unusedLock struct{ resourcelock.Interface }

func TestLeadershipLossFencesCancelsAndReturns(t *testing.T) {
	diagnostics := &Diagnostics{}
	serving := make(chan *Fence, 1)
	canceled := make(chan struct{})
	runner := func(_ context.Context, config leaderelection.LeaderElectionConfig) {
		leaderCtx, cancel := context.WithCancel(context.Background())
		callbackDone := make(chan struct{})
		go func() {
			defer close(callbackDone)
			config.Callbacks.OnStartedLeading(leaderCtx)
		}()
		fence := <-serving
		if err := fence.Check(); err != nil {
			t.Errorf("open leader fence: %v", err)
		}
		cancel()
		config.Callbacks.OnStoppedLeading()
		<-callbackDone
		serving <- fence
	}
	err := Run(context.Background(), Config{
		Lock:           unusedLock{},
		CacheSync:      func(context.Context) bool { return true },
		Diagnostics:    diagnostics,
		ElectionRunner: runner,
		Serve: func(ctx context.Context, fence *Fence) error {
			serving <- fence
			<-ctx.Done()
			close(canceled)
			return nil
		},
	})
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("Run error = %v, want leadership lost", err)
	}
	<-canceled
	state := diagnostics.State()
	if state.Ready || state.Leader || !state.CacheSynced || !state.FormerLeader {
		t.Fatalf("unexpected final state: %#v", state)
	}
	if err := (<-serving).Check(); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("closed fence error = %v", err)
	}
}

func TestLeadershipLossDuringPrepareCannotReviveReadiness(t *testing.T) {
	diagnostics := &Diagnostics{}
	prepareStarted := make(chan struct{})
	releasePrepare := make(chan struct{})
	serveCalled := make(chan struct{}, 1)
	runner := func(_ context.Context, config leaderelection.LeaderElectionConfig) {
		leaderCtx, cancel := context.WithCancel(context.Background())
		callbackDone := make(chan struct{})
		go func() {
			defer close(callbackDone)
			config.Callbacks.OnStartedLeading(leaderCtx)
		}()
		<-prepareStarted
		cancel()
		config.Callbacks.OnStoppedLeading()
		close(releasePrepare)
		<-callbackDone
	}
	err := Run(context.Background(), Config{
		Lock:           unusedLock{},
		CacheSync:      func(context.Context) bool { return true },
		Diagnostics:    diagnostics,
		ElectionRunner: runner,
		Prepare: func(context.Context) error {
			close(prepareStarted)
			<-releasePrepare
			return nil
		},
		Serve: func(context.Context, *Fence) error {
			serveCalled <- struct{}{}
			return nil
		},
	})
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("Run error = %v, want leadership lost", err)
	}
	select {
	case <-serveCalled:
		t.Fatal("Serve started after leadership was lost during Prepare")
	default:
	}
	state := diagnostics.State()
	if state.Ready || state.Leader || !state.FormerLeader {
		t.Fatalf("readiness revived after leadership loss: %#v", state)
	}
}

func TestFenceCannotReopenAfterRevocation(t *testing.T) {
	fence := NewFence()
	if !fence.Open() {
		t.Fatal("new fence did not open")
	}
	fence.Close()
	if fence.Open() || !errors.Is(fence.Check(), ErrNotLeader) {
		t.Fatal("revoked fence reopened")
	}
}

func TestDiagnosticsSeparateLivenessStartupAndReadiness(t *testing.T) {
	diagnostics := &Diagnostics{}
	assertStatus(t, diagnostics.Handler(), "/livez", http.StatusOK)
	assertStatus(t, diagnostics.Handler(), "/startupz", http.StatusServiceUnavailable)
	assertStatus(t, diagnostics.Handler(), "/readyz", http.StatusServiceUnavailable)
	diagnostics.update(func(state *State) { state.CacheSynced = true })
	assertStatus(t, diagnostics.Handler(), "/startupz", http.StatusOK)
	assertStatus(t, diagnostics.Handler(), "/readyz", http.StatusServiceUnavailable)
	diagnostics.update(func(state *State) { state.Leader = true; state.Ready = true })
	assertStatus(t, diagnostics.Handler(), "/readyz", http.StatusOK)
}

func TestCacheSyncFailureNeverRunsElection(t *testing.T) {
	ran := false
	err := Run(context.Background(), Config{
		Lock:      unusedLock{},
		CacheSync: func(context.Context) bool { return false },
		Serve:     func(context.Context, *Fence) error { return nil },
		ElectionRunner: func(context.Context, leaderelection.LeaderElectionConfig) {
			ran = true
		},
	})
	if err == nil || ran {
		t.Fatalf("error = %v, election ran = %v", err, ran)
	}
}

func assertStatus(t *testing.T, handler http.Handler, path string, expected int) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != expected {
		t.Fatalf("%s status = %d, want %d", path, response.Code, expected)
	}
}
