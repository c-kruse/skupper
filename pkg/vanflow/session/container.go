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

// Receiver is not safe for concurrent use.
type Receiver interface {
	// Next blocks until a message is received, retrying across session
	// restarts.
	Next(context.Context) (*amqp.Message, error)
	// Accept settles a message returned by Next.
	Accept(context.Context, *amqp.Message) error
	Close(context.Context) error
}

// Sender is not safe for concurrent use.
type Sender interface {
	// Send blocks until the message is sent or an error is encountered.
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
		address:     address,
		config:      config,
		hasNext:     make(chan struct{}),
		invalidated: make(chan struct{}),
		notifyOK:    make(chan int, 32),
	}
	return c
}

type container struct {
	address string
	config  ContainerConfig

	mu            sync.Mutex
	sess          *amqp.Session
	gen           int
	hasNext       chan struct{}
	errorHandlers []func(error)

	// invalidated is closed when sess is invalidated. Replaced along with sess
	// each time a session is published.
	invalidated chan struct{}
	// sessErr is the error that invalidated sess.
	sessErr error

	notifyOK chan int
}

func (c *container) OnSessionError(handler func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorHandlers = append(c.errorHandlers, handler)
}

// currentSession returns the container's session and its generation, or a nil
// session and a channel that is closed once the next session is published. A
// nil session means the previous one was invalidated and a reconnect is in
// flight. All three values are read together so that a caller cannot observe a
// nil session and then wait on a hasNext that has already been closed.
func (c *container) currentSession() (*amqp.Session, int, chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess, c.gen, c.hasNext
}

// invalidate discards the session at generation gen, so that the container's
// links stop using it before they discover the failure themselves, and wakes
// the container to reconnect. Invalidating is idempotent: the generation check
// discards a report for a session that has already been replaced, so a link
// failing late cannot tear down the healthy session that succeeded it, and
// concurrent failures on the same session cause a single reconnect.
func (c *container) invalidate(gen int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || c.sess == nil {
		return
	}
	c.sess, c.sessErr = nil, err
	close(c.invalidated)
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
		var generation int
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
				generation++

				c.mu.Lock()
				close(c.hasNext)
				c.sess, c.gen = sess, generation
				c.hasNext, c.invalidated = make(chan struct{}), make(chan struct{})
				c.sessErr = nil
				invalidated := c.invalidated
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
					case gen := <-c.notifyOK:
						if gen == generation {
							b.Reset()
						}
					case <-invalidated:
						c.mu.Lock()
						sessErr := c.sessErr
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

// link holds no session of its own. It attaches its sender or receiver to
// whichever session the container has published, remembering the generation it
// attached to so that it can tell when the container has moved on.
type link struct {
	address      string
	receiverOpts amqp.ReceiverOptions
	senderOpts   amqp.SenderOptions

	reportOK chan<- int

	container *container

	mu     sync.Mutex
	closed bool
	rcvGen int
	rcv    *amqp.Receiver
	sndGen int
	snd    *amqp.Sender
}

var (
	errLinkClosed    = errors.New("link closed")
	errStaleDelivery = errors.New("delivery belongs to a closed session")
)

// session waits for the container to have a valid session and returns it along
// with its generation. Blocks for the duration of a reconnect rather than
// handing back a session known to be dead.
func (r *link) session(ctx context.Context) (*amqp.Session, int, error) {
	for {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return nil, 0, errLinkClosed
		}
		sess, gen, hasNext := r.container.currentSession()
		if sess != nil {
			return sess, gen, nil
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-hasNext: // re-read: the new session may already be invalid
		}
	}
}

// handleError detaches whatever this link had attached to the failed session
// and invalidates that session on the container, so that the container's other
// links stop using it without each having to fail first.
func (r *link) handleError(ctx context.Context, gen int, err error) error {
	if errors.Is(err, ctx.Err()) {
		return err
	}
	if errors.Is(err, errLinkClosed) {
		return err
	}
	r.mu.Lock()
	// only drop links attached to the failing session. A concurrent operation
	// may have already reattached this link to a newer one.
	if r.sndGen == gen {
		r.snd = nil
	}
	if r.rcvGen == gen {
		r.rcv = nil
	}
	r.mu.Unlock()

	r.container.invalidate(gen, err)
	return err
}

func (r *link) getReceiver(ctx context.Context, sess *amqp.Session, gen int) (*amqp.Receiver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errLinkClosed
	}
	if r.rcv != nil && r.rcvGen == gen {
		return r.rcv, nil
	}
	rcv, err := sess.NewReceiver(ctx, r.address, &amqp.ReceiverOptions{Credit: int32(r.receiverOpts.Credit)})
	if err != nil {
		return nil, err
	}
	r.rcv, r.rcvGen = rcv, gen
	return r.rcv, nil
}

func (r *link) getSender(ctx context.Context, sess *amqp.Session, gen int) (*amqp.Sender, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errLinkClosed
	}
	if r.snd != nil && r.sndGen == gen {
		return r.snd, nil
	}
	snd, err := sess.NewSender(ctx, r.address, &r.senderOpts)
	if err != nil {
		return nil, err
	}
	r.snd, r.sndGen = snd, gen
	return r.snd, nil
}

func (r *link) Next(ctx context.Context) (*amqp.Message, error) {
	for {
		sess, gen, err := r.session(ctx)
		if err != nil {
			return nil, err
		}
		rcv, err := r.getReceiver(ctx, sess, gen)
		if err != nil {
			err = r.handleError(ctx, gen, fmt.Errorf("receiver create error: %w", err))
		} else {
			var msg *amqp.Message
			if msg, err = rcv.Receive(ctx, nil); err == nil {
				return msg, nil
			}
			err = r.handleError(ctx, gen, fmt.Errorf("receive error: %w", err))
		}
		if ctx.Err() != nil || errors.Is(err, errLinkClosed) {
			return nil, err
		}
		// the session has been invalidated: wait for its replacement and retry
	}
}

func (r *link) Accept(ctx context.Context, msg *amqp.Message) error {
	r.mu.Lock()
	rcv, gen, closed := r.rcv, r.rcvGen, r.closed
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
	case r.reportOK <- gen:
	default:
	}
	return nil
}

func (r *link) Send(ctx context.Context, msg *amqp.Message) error {
	sess, gen, err := r.session(ctx)
	if err != nil {
		return err
	}
	snd, err := r.getSender(ctx, sess, gen)
	if err != nil {
		return r.handleError(ctx, gen, fmt.Errorf("sender create error: %w", err))
	}
	if err := snd.Send(ctx, msg, nil); err != nil {
		return r.handleError(ctx, gen, fmt.Errorf("send error: %w", err))
	}
	select {
	case r.reportOK <- gen:
	default:
	}
	return nil
}

func (r *link) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	rcv, snd := r.rcv, r.snd
	r.rcv, r.snd = nil, nil
	var errs []error
	if rcv != nil {
		errs = append(errs, rcv.Close(ctx))
	}
	if snd != nil {
		errs = append(errs, snd.Close(ctx))
	}
	return errors.Join(errs...)
}
