package leadership

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	DefaultLeaseDuration = 30 * time.Second
	DefaultRenewDeadline = 20 * time.Second
	DefaultRetryPeriod   = 5 * time.Second
)

var ErrLeadershipLost = errors.New("controller leadership was lost")

type State struct {
	CacheSynced  bool
	Leader       bool
	Ready        bool
	FormerLeader bool
	LastError    string
}

type Diagnostics struct {
	mu    sync.RWMutex
	state State
}

func (d *Diagnostics) State() State {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

func (d *Diagnostics) update(change func(*State)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	change(&d.state)
}

// Handler exposes cheap, non-sensitive lifecycle probes. /startupz is evidence
// that startup cache synchronization completed; it is not a continuous cache
// health assertion. /readyz is leader-only.
func (d *Diagnostics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/startupz", func(w http.ResponseWriter, _ *http.Request) {
		if !d.State().CacheSynced {
			http.Error(w, "not synchronized", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !d.State().Ready {
			http.Error(w, "not serving", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

type Config struct {
	Lock           resourcelock.Interface
	CacheSync      func(context.Context) bool
	Prepare        func(context.Context) error
	Serve          func(context.Context, *Fence) error
	Diagnostics    *Diagnostics
	ElectionRunner func(context.Context, leaderelection.LeaderElectionConfig)
	LeaseDuration  time.Duration
	RenewDeadline  time.Duration
	RetryPeriod    time.Duration
}

// Run participates in exactly one election. A process that has led returns on
// leadership loss and must exit; it never rejoins the election as a standby.
func Run(ctx context.Context, config Config) error {
	if config.Lock == nil || config.CacheSync == nil || config.Serve == nil {
		return fmt.Errorf("leadership is not configured")
	}
	diagnostics := config.Diagnostics
	if diagnostics == nil {
		diagnostics = &Diagnostics{}
	}
	if !config.CacheSync(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("required informer caches did not synchronize")
	}
	diagnostics.update(func(state *State) { state.CacheSynced = true })

	leaseDuration := config.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = DefaultLeaseDuration
	}
	renewDeadline := config.RenewDeadline
	if renewDeadline == 0 {
		renewDeadline = DefaultRenewDeadline
	}
	retryPeriod := config.RetryPeriod
	if retryPeriod == 0 {
		retryPeriod = DefaultRetryPeriod
	}
	electionCtx, stopElection := context.WithCancel(ctx)
	defer stopElection()
	fence := NewFence()
	var mu sync.Mutex
	var runErr error
	becameLeader := false

	runElection := config.ElectionRunner
	if runElection == nil {
		runElection = func(ctx context.Context, electionConfig leaderelection.LeaderElectionConfig) {
			elector, err := leaderelection.NewLeaderElector(electionConfig)
			if err != nil {
				mu.Lock()
				runErr = fmt.Errorf("configure leader election: %w", err)
				mu.Unlock()
				return
			}
			elector.Run(ctx)
		}
	}
	runElection(electionCtx, leaderelection.LeaderElectionConfig{
		Lock:          config.Lock,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		// Do not release before leader work has observed cancellation. Allowing
		// the Lease to expire is safer than making a successor active while an
		// in-flight effect from this process can still complete.
		ReleaseOnCancel: false,
		Name:            "skupper-controller",
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				mu.Lock()
				becameLeader = true
				mu.Unlock()
				diagnostics.update(func(state *State) { state.Leader = true })
				go func() {
					<-leaderCtx.Done()
					fence.Close()
					diagnostics.update(func(state *State) { state.Ready = false })
				}()
				if config.Prepare != nil {
					if err := config.Prepare(leaderCtx); err != nil {
						if leaderCtx.Err() != nil {
							return
						}
						mu.Lock()
						runErr = fmt.Errorf("initialize leader serving prerequisites: %w", err)
						mu.Unlock()
						diagnostics.update(func(state *State) { state.LastError = err.Error() })
						stopElection()
						return
					}
				}
				fence.Open()
				diagnostics.update(func(state *State) { state.Ready = true })
				if err := config.Serve(leaderCtx, fence); leaderCtx.Err() == nil {
					if err == nil {
						err = fmt.Errorf("leader serving stopped unexpectedly")
					}
					mu.Lock()
					runErr = fmt.Errorf("leader serving failed: %w", err)
					mu.Unlock()
					diagnostics.update(func(state *State) { state.LastError = err.Error() })
					fence.Close()
					diagnostics.update(func(state *State) { state.Ready = false })
					stopElection()
				}
			},
			OnStoppedLeading: func() {
				fence.Close()
				mu.Lock()
				led := becameLeader
				mu.Unlock()
				diagnostics.update(func(state *State) {
					state.Leader = false
					state.Ready = false
					state.FormerLeader = led
				})
			},
		},
	})
	fence.Close()
	diagnostics.update(func(state *State) { state.Leader = false; state.Ready = false })
	mu.Lock()
	err := runErr
	wasLeader := becameLeader
	mu.Unlock()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if wasLeader {
		return ErrLeadershipLost
	}
	return fmt.Errorf("leader election stopped before leadership")
}
