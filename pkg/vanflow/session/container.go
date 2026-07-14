// session implements amqp connection and session management though the concept
// of a Container, inspired by the Container interface exposed by the qpid
// proton amqp libraries. This abstraction allows vanflow components to be
// written without repeating connection management tasks.
package session

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/go-amqp"
	"github.com/cenkalti/backoff/v4"
)

type ReceiverOptions struct {
	Credit int
}

func (o ReceiverOptions) get() amqp.ReceiverOptions {
	var result amqp.ReceiverOptions
	if o.Credit > 0 {
		result.Credit = int32(o.Credit)
	}
	return result
}

type SenderOptions struct {
}

func (o SenderOptions) get() amqp.SenderOptions {
	var result amqp.SenderOptions
	return result
}

type Container interface {
	// Start the container
	Start(context.Context)
	// NewReceiver adds a new receiver link using the container's session
	NewReceiver(address string, opts ReceiverOptions) Receiver
	// NewSender adds a new sender link using the container's session
	NewSender(address string, opts SenderOptions) Sender
	// OnSessionError
	OnSessionError(func(err error))
}

type RetryableError interface {
	Retry() time.Duration
}

type Receiver interface {
	Next(context.Context) (*amqp.Message, error)
	Accept(context.Context, *amqp.Message) error
	Close(context.Context) error
}

type Sender interface {
	Send(context.Context, *amqp.Message) error
	Close(context.Context) error
}

type SASLType string

const (
	SASLTypeExternal SASLType = "EXTERNAL"
)

type ContainerConfig struct {
	ContainerID  string
	MaxFrameSize uint32
	TLSConfig    *tls.Config
	SASLType     SASLType
	// BackOff strategy to use when reestablishing a connection defaults to an
	// exponential backoff capped at 30 second intervals with no set retry
	// limit.
	BackOff backoff.BackOff
}

func (cfg ContainerConfig) toAmqp() *amqp.ConnOptions {
	opts := amqp.ConnOptions{
		ContainerID:  cfg.ContainerID,
		MaxFrameSize: cfg.MaxFrameSize,
		TLSConfig:    cfg.TLSConfig,
	}
	switch cfg.SASLType {
	case SASLTypeExternal:
		opts.SASLType = amqp.SASLTypeExternal("")
	}
	return &opts
}

// NewContainer creates an amqp container that will attempt to create a single
// connection + session pair using the supplied amqp connection options for use
// with the container's Senders and Receivers. Will recreate the connection and
// session when a link encounters an error using the specified backoff
// strategy.
func NewContainer(address string, config ContainerConfig) Container {
	c := &container{
		address:  address,
		config:   config,
		state:    &sessionState{done: make(chan struct{})},
		notifyOK: make(chan *sessionState, 32),
	}
	return c
}

type sessionState struct {
	sess *amqp.Session
	err  error
	done chan struct{}
}

type container struct {
	address string
	config  ContainerConfig

	mu            sync.Mutex
	state         *sessionState
	errorHandlers []func(error)

	notifyOK chan *sessionState
}

func (c *container) OnSessionError(handler func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorHandlers = append(c.errorHandlers, handler)
}

func (c *container) current() *sessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// invalidate discards the session held by state and wakes the container to
// reconnect.
func (c *container) invalidate(state *sessionState, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != state || state.sess == nil {
		return
	}
	c.state = &sessionState{err: err, done: make(chan struct{})}
	close(state.done)
}

// Start the container. It will run until the context is cancelled or until the
// backoff strategy chosen finishes.
func (c *container) Start(ctx context.Context) {
	if c.config.BackOff == nil {
		b := backoff.NewExponentialBackOff()
		b.InitialInterval = time.Millisecond * 250
		b.MaxInterval = time.Second * 30
		b.MaxElapsedTime = 0
		b.Reset()
		c.config.BackOff = b
	}

	go func() {
		var prevSessionTeardown func() = func() {}
		b := backoff.WithContext(c.config.BackOff, ctx)
		err := backoff.RetryNotify(
			func() error {
				conn, err := amqp.Dial(ctx, c.address, c.config.toAmqp())
				if err != nil {
					return fmt.Errorf("dial error: %s", err)
				}
				sess, err := conn.NewSession(ctx, nil)
				if err != nil {
					return fmt.Errorf("session create error: %s", err)
				}

				published := &sessionState{sess: sess, done: make(chan struct{})}
				c.mu.Lock()
				prev := c.state
				c.state = published
				close(prev.done)
				c.mu.Unlock()

				prevSessionTeardown()
				prevSessionTeardown = func() {
					sess.Close(ctx)
					conn.Close()
				}

				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case state := <-c.notifyOK:
						if state == published {
							b.Reset()
						}
					case <-published.done:
						// only invalidate can supersede the state this loop
						// published, so the current state carries the error
						c.mu.Lock()
						sessErr := c.state.err
						c.mu.Unlock()
						return fmt.Errorf("session receiver error: %s", sessErr)
					}
				}
			},
			b,
			func(err error, d time.Duration) {
				wErr := errSessionRestart{Err: err, D: d}
				c.mu.Lock()
				for _, handler := range c.errorHandlers {
					handler(wErr)
				}
				c.mu.Unlock()
			},
		)
		defer prevSessionTeardown()
		if err != nil {
			if errors.Is(err, ctx.Err()) {
				return
			}
			wErr := fmt.Errorf("error caused container to close: %w", err)
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, handler := range c.errorHandlers {
				handler(wErr)
			}
		}
	}()
}

type errSessionRestart struct {
	Err error
	D   time.Duration
}

func (e errSessionRestart) Error() string {
	return fmt.Sprintf("session error: %s", e.Err)
}

func (e errSessionRestart) Retry() time.Duration {
	return e.D
}

func (s *container) NewReceiver(address string, opts ReceiverOptions) Receiver {
	return s.newLink(address, opts, SenderOptions{})
}

func (s *container) NewSender(address string, opts SenderOptions) Sender {
	return s.newLink(address, ReceiverOptions{}, opts)
}

func (c *container) newLink(address string, r ReceiverOptions, s SenderOptions) *link {
	return &link{
		address:      address,
		container:    c,
		receiverOpts: r.get(),
		senderOpts:   s.get(),
		reportOK:     c.notifyOK,
	}
}

type link struct {
	address      string
	receiverOpts amqp.ReceiverOptions
	senderOpts   amqp.SenderOptions

	reportOK chan<- *sessionState

	container *container

	mu       sync.Mutex
	closed   bool
	rcvState *sessionState
	rcv      *amqp.Receiver
	sndState *sessionState
	snd      *amqp.Sender
}

var (
	errLinkClosed    = errors.New("link closed")
	errStaleDelivery = errors.New("delivery belongs to a closed session")
)

// session blocks for a live session.
func (r *link) session(ctx context.Context) (*sessionState, error) {
	for {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return nil, errLinkClosed
		}
		state := r.container.current()
		if state.sess != nil {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-state.done: // re-read: the new state may already be invalid
		}
	}
}

func (r *link) handleError(ctx context.Context, state *sessionState, err error) error {
	if errors.Is(err, ctx.Err()) {
		return err
	}
	if errors.Is(err, errLinkClosed) {
		return err
	}
	r.mu.Lock()
	r.rcv, r.rcvState = nil, nil
	r.snd, r.sndState = nil, nil
	r.mu.Unlock()

	r.container.invalidate(state, err)
	return err
}

func (r *link) getReceiver(ctx context.Context, state *sessionState) (*amqp.Receiver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errLinkClosed
	}
	if r.rcv != nil && r.rcvState == state {
		return r.rcv, nil
	}
	rcv, err := state.sess.NewReceiver(ctx, r.address, &amqp.ReceiverOptions{Credit: int32(r.receiverOpts.Credit)})
	if err != nil {
		return nil, err
	}
	r.rcv, r.rcvState = rcv, state
	return r.rcv, nil
}

func (r *link) getSender(ctx context.Context, state *sessionState) (*amqp.Sender, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errLinkClosed
	}
	if r.snd != nil && r.sndState == state {
		return r.snd, nil
	}
	snd, err := state.sess.NewSender(ctx, r.address, &r.senderOpts)
	if err != nil {
		return nil, err
	}
	r.snd, r.sndState = snd, state
	return r.snd, nil
}

func (r *link) Next(ctx context.Context) (*amqp.Message, error) {
	for {
		state, err := r.session(ctx)
		if err != nil {
			return nil, err
		}
		rcv, err := r.getReceiver(ctx, state)
		if err != nil {
			err = r.handleError(ctx, state, fmt.Errorf("receiver create error: %w", err))
		} else {
			var msg *amqp.Message
			if msg, err = rcv.Receive(ctx, nil); err == nil {
				return msg, nil
			}
			err = r.handleError(ctx, state, fmt.Errorf("receive error: %w", err))
		}
		if ctx.Err() != nil || errors.Is(err, errLinkClosed) {
			return nil, err
		}
		// the session has been invalidated: wait for its replacement and retry
	}
}

func (r *link) Accept(ctx context.Context, msg *amqp.Message) error {
	r.mu.Lock()
	rcv, state, closed := r.rcv, r.rcvState, r.closed
	r.mu.Unlock()
	if closed {
		return errLinkClosed
	}
	if rcv == nil {
		return errStaleDelivery
	}
	if err := rcv.AcceptMessage(ctx, msg); err != nil {
		return err
	}
	select {
	case r.reportOK <- state:
	default:
	}
	return nil
}

func (r *link) Send(ctx context.Context, msg *amqp.Message) error {
	state, err := r.session(ctx)
	if err != nil {
		return err
	}
	snd, err := r.getSender(ctx, state)
	if err != nil {
		return r.handleError(ctx, state, fmt.Errorf("sender create error: %w", err))
	}
	if err := snd.Send(ctx, msg, nil); err != nil {
		return r.handleError(ctx, state, fmt.Errorf("send error: %w", err))
	}
	select {
	case r.reportOK <- state:
	default:
	}
	return nil
}

func (r *link) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	rcv, snd := r.rcv, r.snd
	r.rcv, r.rcvState = nil, nil
	r.snd, r.sndState = nil, nil
	var errs []error
	if rcv != nil {
		errs = append(errs, rcv.Close(ctx))
	}
	if snd != nil {
		errs = append(errs, snd.Close(ctx))
	}
	return errors.Join(errs...)
}
