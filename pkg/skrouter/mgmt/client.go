package mgmt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/Azure/go-amqp"
)

const defaultMaxInflight = 32

// Dialer establishes an AMQP connection. TLS, SASL, and address selection are
// owned by the caller.
type Dialer func(context.Context) (*amqp.Conn, error)

// Option configures a Client.
type Option func(*Client)

// WithRequestTimeout sets the default timeout applied when a caller's context
// has no earlier deadline. A non-positive duration disables the default.
func WithRequestTimeout(timeout time.Duration) Option {
	return func(client *Client) { client.requestTimeout = timeout }
}

// WithMaxInflight sets both the concurrent request limit and receiver credit.
func WithMaxInflight(maximum int) Option {
	return func(client *Client) {
		if maximum > 0 {
			client.maxInflight = maximum
		}
	}
}

// WithLogger sets the structured logger used by the client.
func WithLogger(logger *slog.Logger) Option {
	return func(client *Client) {
		if logger != nil {
			client.logger = logger
		}
	}
}

// WithMutationLogLevel sets the level for CREATE, UPDATE, and DELETE logs.
func WithMutationLogLevel(level slog.Level) Option {
	return func(client *Client) { client.mutationLogLevel = level }
}

// Client is a lazy, concurrent router management client.
type Client struct {
	dial             Dialer
	requestTimeout   time.Duration
	maxInflight      int
	logger           *slog.Logger
	mutationLogLevel slog.Level

	mu      sync.Mutex
	state   *clientState
	dialing *dialAttempt
	closed  bool
	nextID  atomic.Uint64
	slots   chan struct{}
}

type dialAttempt struct {
	done    chan struct{}
	cancel  context.CancelFunc
	state   *clientState
	err     error
	waiters int
}

type clientState struct {
	conn     *amqp.Conn
	session  *amqp.Session
	sender   *amqp.Sender
	receiver *amqp.Receiver
	replyTo  string
	ctx      context.Context
	cancel   context.CancelFunc
	pending  map[uint64]chan callResult
}

type callResult struct {
	message *amqp.Message
	err     error
}

// NewClient creates a client. The dialer is not called until the first request.
func NewClient(dial Dialer, options ...Option) *Client {
	client := &Client{
		dial:             dial,
		requestTimeout:   10 * time.Second,
		maxInflight:      defaultMaxInflight,
		logger:           slog.Default().With("component", "skrouter.mgmt"),
		mutationLogLevel: slog.LevelInfo,
	}
	for _, option := range options {
		option(client)
	}
	client.slots = make(chan struct{}, client.maxInflight)
	return client
}

// Target is a router management destination.
type Target struct {
	client  *Client
	address string
}

// Address returns the AMQP management destination.
func (t Target) Address() string { return t.address }

// Local returns the management target for the directly connected router.
func (c *Client) Local() Target { return Target{client: c, address: "$management"} }

// Interior returns the topological management target for an interior router.
func (c *Client) Interior(routerID string) Target {
	return Target{client: c, address: "_topo/0/" + routerID + "/$management"}
}

// Edge returns the topological management target for an edge router.
func (c *Client) Edge(routerID string) Target {
	return Target{client: c, address: "_edge/" + routerID + "/$management"}
}

// Request is the raw management protocol escape hatch.
type Request struct {
	Operation  string
	Type       string
	Ref        Ref
	Body       any
	Count      int
	Offset     int
	Properties map[string]any
}

// Response is a successful raw management response.
type Response struct {
	StatusCode        int
	StatusDescription string
	Body              any
}

// Do performs a raw management request.
func (t Target) Do(ctx context.Context, request *Request) (*Response, error) {
	if t.client == nil {
		return nil, errors.New("management target has no client")
	}
	if request == nil {
		return nil, errors.New("management request is nil")
	}
	if request.Operation == "" {
		return nil, errors.New("management request operation is empty")
	}
	if request.Ref.Name != "" && request.Ref.Identity != "" {
		return nil, errors.New("management request cannot set both name and identity")
	}
	return t.client.do(ctx, t.address, request)
}

func (c *Client) do(ctx context.Context, address string, request *Request) (*Response, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	state, err := c.getState(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	id := c.nextID.Add(1)
	result := make(chan callResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.state != state {
		c.mu.Unlock()
		return nil, &TransportError{Err: errors.New("management connection changed before send")}
	}
	state.pending[id] = result
	c.mu.Unlock()

	message := requestMessage(address, state.replyTo, id, request)
	if isMutation(request.Operation) {
		c.logger.Log(ctx, c.mutationLogLevel, "Management request",
			"operation", request.Operation,
			"type", request.Type,
			"ref", request.Ref.String())
	}
	if err := state.sender.Send(ctx, message, &amqp.SendOptions{Settled: true}); err != nil {
		if ctx.Err() != nil {
			c.removePending(state, id)
			return nil, ctx.Err()
		}
		c.failState(state, err)
		return nil, &TransportError{Err: err}
	}

	select {
	case outcome := <-result:
		if outcome.err != nil {
			return nil, outcome.err
		}
		return parseResponse(outcome.message, request)
	case <-ctx.Done():
		c.removePending(state, id)
		return nil, ctx.Err()
	}
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

func (c *Client) getState(ctx context.Context) (*clientState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.state != nil {
		state := c.state
		c.mu.Unlock()
		return state, nil
	}
	attempt := c.dialing
	if attempt == nil {
		if c.dial == nil {
			c.mu.Unlock()
			return nil, &TransportError{Err: errors.New("management dialer is nil")}
		}
		dialCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		attempt = &dialAttempt{done: make(chan struct{}), cancel: cancel}
		c.dialing = attempt
		go c.runDial(dialCtx, attempt)
	}
	attempt.waiters++
	c.mu.Unlock()

	select {
	case <-attempt.done:
		c.releaseDialWaiter(attempt)
		return attempt.state, attempt.err
	case <-ctx.Done():
		c.releaseDialWaiter(attempt)
		return nil, ctx.Err()
	}
}

func (c *Client) releaseDialWaiter(attempt *dialAttempt) {
	c.mu.Lock()
	attempt.waiters--
	if attempt.waiters == 0 && c.dialing == attempt {
		c.dialing = nil
		attempt.cancel()
	}
	c.mu.Unlock()
}

func (c *Client) runDial(ctx context.Context, attempt *dialAttempt) {
	defer attempt.cancel()
	state, err := c.openState(ctx)
	if err != nil {
		err = &TransportError{Err: err}
	}

	c.mu.Lock()
	if c.dialing != attempt {
		c.mu.Unlock()
		if state != nil {
			state.cancel()
			_ = state.conn.Close()
		}
		return
	}
	c.dialing = nil
	if c.closed {
		attempt.err = ErrClosed
	} else if err != nil {
		attempt.err = err
	} else {
		attempt.state = state
		c.state = state
	}
	close(attempt.done)
	published := c.state == state && state != nil
	c.mu.Unlock()

	if published {
		go c.receive(state)
	} else if state != nil {
		state.cancel()
		_ = state.conn.Close()
	}
}

func (c *Client) openState(ctx context.Context) (*clientState, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("management dialer returned a nil connection")
	}
	session, err := conn.NewSession(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	sender, err := session.NewSender(ctx, "", nil)
	if err != nil {
		_ = session.Close(ctx)
		_ = conn.Close()
		return nil, err
	}
	receiver, err := session.NewReceiver(ctx, "", &amqp.ReceiverOptions{
		Credit:         int32(c.maxInflight),
		DynamicAddress: true,
	})
	if err != nil {
		_ = sender.Close(ctx)
		_ = session.Close(ctx)
		_ = conn.Close()
		return nil, err
	}
	stateCtx, cancel := context.WithCancel(context.Background())
	state := &clientState{
		conn: conn, session: session, sender: sender, receiver: receiver,
		replyTo: receiver.Address(), ctx: stateCtx, cancel: cancel,
		pending: make(map[uint64]chan callResult),
	}
	return state, nil
}

func (c *Client) receive(state *clientState) {
	for {
		message, err := state.receiver.Receive(state.ctx, nil)
		if err != nil {
			if state.ctx.Err() == nil {
				c.failState(state, err)
			}
			return
		}
		if err := state.receiver.AcceptMessage(state.ctx, message); err != nil {
			if state.ctx.Err() == nil {
				c.failState(state, err)
			}
			return
		}
		if message.Properties == nil {
			c.logger.Warn("Ignoring management response with no message properties")
			continue
		}
		id, ok := AsUint64(message.Properties.CorrelationID)
		if !ok {
			c.logger.Warn("Ignoring management response with invalid correlation-id", "type", fmt.Sprintf("%T", message.Properties.CorrelationID))
			continue
		}

		c.mu.Lock()
		result := state.pending[id]
		delete(state.pending, id)
		c.mu.Unlock()
		if result == nil {
			c.logger.Debug("Ignoring management response with unknown correlation-id", "correlation_id", id)
			continue
		}
		result <- callResult{message: message}
	}
}

func (c *Client) removePending(state *clientState, id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == state {
		delete(state.pending, id)
	}
}

func (c *Client) failState(state *clientState, cause error) {
	transportErr := &TransportError{Err: cause}
	c.mu.Lock()
	if c.state != state {
		c.mu.Unlock()
		return
	}
	c.state = nil
	state.cancel()
	pending := state.pending
	state.pending = make(map[uint64]chan callResult)
	c.mu.Unlock()
	_ = state.conn.Close()
	for _, result := range pending {
		result <- callResult{err: transportErr}
	}
}

// Close fails pending calls and closes any established AMQP resources.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	attempt := c.dialing
	c.dialing = nil
	if attempt != nil {
		attempt.err = ErrClosed
		attempt.cancel()
		close(attempt.done)
	}
	state := c.state
	c.state = nil
	var pending map[uint64]chan callResult
	if state != nil {
		state.cancel()
		pending = state.pending
		state.pending = make(map[uint64]chan callResult)
	}
	c.mu.Unlock()
	for _, result := range pending {
		result <- callResult{err: ErrClosed}
	}
	if state == nil {
		return nil
	}
	return errors.Join(
		state.receiver.Close(ctx),
		state.sender.Close(ctx),
		state.session.Close(ctx),
		state.conn.Close(),
	)
}

func requestMessage(address, replyTo string, id uint64, request *Request) *amqp.Message {
	properties := make(map[string]any, len(request.Properties)+5)
	for key, value := range request.Properties {
		properties[key] = value
	}
	properties["operation"] = request.Operation
	if request.Type != "" {
		if request.Operation == "QUERY" {
			properties["entityType"] = request.Type
		} else {
			properties["type"] = request.Type
		}
	}
	if request.Ref.Name != "" {
		properties["name"] = request.Ref.Name
	}
	if request.Ref.Identity != "" {
		properties["identity"] = request.Ref.Identity
	}
	if request.Count != 0 {
		properties["count"] = int64(request.Count)
	}
	if request.Offset != 0 {
		properties["offset"] = int64(request.Offset)
	}
	return &amqp.Message{
		Properties: &amqp.MessageProperties{
			To:            &address,
			ReplyTo:       &replyTo,
			CorrelationID: id,
		},
		ApplicationProperties: properties,
		Value:                 request.Body,
	}
}

func parseResponse(message *amqp.Message, request *Request) (*Response, error) {
	code64, ok := AsInt64(message.ApplicationProperties["statusCode"])
	if !ok {
		return nil, fmt.Errorf("management response has invalid statusCode %T", message.ApplicationProperties["statusCode"])
	}
	description, _ := AsString(message.ApplicationProperties["statusDescription"])
	code := int(code64)
	if code < 200 || code >= 300 {
		return nil, &StatusError{
			Code: code, Description: description, Operation: request.Operation,
			Type: request.Type, Ref: request.Ref,
		}
	}
	return &Response{StatusCode: code, StatusDescription: description, Body: message.Value}, nil
}

func isMutation(operation string) bool {
	switch operation {
	case "CREATE", "UPDATE", "DELETE":
		return true
	default:
		return false
	}
}

// Interiors discovers interior management nodes reachable from the local
// router.
func (c *Client) Interiors(ctx context.Context) ([]Target, error) {
	response, err := c.Local().Do(ctx, &Request{Operation: "GET-MGMT-NODES"})
	if err != nil {
		return nil, err
	}
	addresses, ok := response.Body.([]any)
	if !ok {
		if typed, typedOK := response.Body.([]string); typedOK {
			result := make([]Target, len(typed))
			for i, address := range typed {
				result[i] = Target{client: c, address: managementNodeAddress(address)}
			}
			return result, nil
		}
		return nil, fmt.Errorf("GET-MGMT-NODES returned %T, want list", response.Body)
	}
	result := make([]Target, len(addresses))
	for i, item := range addresses {
		address, ok := AsString(item)
		if !ok {
			return nil, fmt.Errorf("GET-MGMT-NODES item %d is %T, want string", i, item)
		}
		result[i] = Target{client: c, address: managementNodeAddress(address)}
	}
	return result, nil
}

func managementNodeAddress(address string) string {
	return strings.TrimPrefix(address, "amqp:/")
}
