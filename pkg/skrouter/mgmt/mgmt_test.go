package mgmt

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/Azure/go-amqp"
)

type testEntity struct {
	Name    string
	Count   int64
	Enabled bool
}

type observedDoneContext struct {
	context.Context
	observed chan<- struct{}
}

func (c observedDoneContext) Done() <-chan struct{} {
	select {
	case c.observed <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

const (
	testName Field[testEntity] = iota
	testCount
	testEnabled
)

var testEntityType = NewType(
	"io.skupper.router.test", "test", false,
	[]string{"QUERY", "READ", "CREATE", "UPDATE", "DELETE"},
	[]Attribute{
		{Name: "name", Kind: StringAttribute, Create: true},
		{Name: "count", Kind: Int64Attribute, Update: true},
		{Name: "enabled", Kind: BoolAttribute, Create: true, Update: true},
	},
)

func (*testEntity) ManagementType() *Type { return testEntityType }

func (e *testEntity) SetManagementField(field Field[testEntity], value any) error {
	switch field {
	case testName:
		decoded, ok := AsString(value)
		if !ok {
			return DecodeError(testEntityType.Name, "name", value)
		}
		e.Name = decoded
	case testCount:
		decoded, ok := AsInt64(value)
		if !ok {
			return DecodeError(testEntityType.Name, "count", value)
		}
		e.Count = decoded
	case testEnabled:
		decoded, ok := AsBool(value)
		if !ok {
			return DecodeError(testEntityType.Name, "enabled", value)
		}
		e.Enabled = decoded
	}
	return nil
}

func (e *testEntity) ManagementValue(field Field[testEntity]) any {
	switch field {
	case testName:
		return e.Name
	case testCount:
		return e.Count
	case testEnabled:
		return e.Enabled
	default:
		return nil
	}
}

func TestFieldSetAndAttributes(t *testing.T) {
	selected := Fields(testEnabled, testName)
	if !selected.Has(testName) || !selected.Has(testEnabled) || selected.Has(testCount) {
		t.Fatalf("unexpected selected fields: %v", Names[testEntity](selected))
	}
	if got, want := Names[testEntity](selected), []string{"name", "enabled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if got := selected.Intersect(Fields(testName, testCount)); got != Fields(testName) {
		t.Fatalf("Intersect() = %v", Names[testEntity](got))
	}
	if got := selected.Without(Fields(testEnabled)); got != Fields(testName) {
		t.Fatalf("Without() = %v", Names[testEntity](got))
	}
	if got, found := TypeByName(testEntityType.Name); !found || got != testEntityType {
		t.Fatalf("TypeByName(%q) = %p, %t", testEntityType.Name, got, found)
	}

	partial := Partial[testEntity]{
		Value:  testEntity{Name: "alpha", Enabled: false},
		Fields: Fields(testName, testEnabled),
	}
	var names []string
	var values []any
	for name, value := range Attributes[testEntity](partial) {
		names = append(names, name)
		values = append(values, value)
	}
	if want := []string{"name", "enabled"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("Attributes names = %v, want %v", names, want)
	}
	if want := []any{"alpha", false}; !reflect.DeepEqual(values, want) {
		t.Fatalf("Attributes values = %v, want %v", values, want)
	}
}

func TestRequestMessageUsesManagementProtocolProperties(t *testing.T) {
	request := &Request{
		Operation: "QUERY",
		Type:      "io.skupper.router.listener",
		Body:      map[string]any{"attributeNames": []string{"name"}},
		Properties: map[string]any{
			"offset": int64(0),
			"count":  int64(25),
		},
	}
	message := requestMessage("_topo/0/router-a/$management", "dynamic-reply", 42, request)
	if got := *message.Properties.To; got != "_topo/0/router-a/$management" {
		t.Fatalf("To = %q", got)
	}
	if got := *message.Properties.ReplyTo; got != "dynamic-reply" {
		t.Fatalf("ReplyTo = %q", got)
	}
	if got := message.Properties.CorrelationID; got != uint64(42) {
		t.Fatalf("CorrelationID = %#v", got)
	}
	if got := message.ApplicationProperties["operation"]; got != "QUERY" {
		t.Fatalf("operation = %#v", got)
	}
	if got := message.ApplicationProperties["entityType"]; got != request.Type {
		t.Fatalf("entityType = %#v", got)
	}
	if _, found := message.ApplicationProperties["type"]; found {
		t.Fatal("QUERY unexpectedly contains type property")
	}
	if got := message.ApplicationProperties["offset"]; got != int64(0) {
		t.Fatalf("offset = %#v", got)
	}

	create := requestMessage("$management", "dynamic-reply", 43, &Request{
		Operation: "CREATE",
		Type:      "io.skupper.router.tcpConnector",
		Ref:       ByName("owned-connector"),
	})
	if got := create.ApplicationProperties["name"]; got != "owned-connector" {
		t.Fatalf("CREATE name = %#v", got)
	}
	if got := create.ApplicationProperties["type"]; got != "io.skupper.router.tcpConnector" {
		t.Fatalf("CREATE type = %#v", got)
	}
}

func TestManagementNodeAddressMatchesInteriorTarget(t *testing.T) {
	client := NewClient(nil)
	wireAddress := "amqp:/_topo/0/router-a/$management"
	if got, want := managementNodeAddress(wireAddress), client.Interior("router-a").Address(); got != want {
		t.Fatalf("management node address = %q, want %q", got, want)
	}
}

func TestQueryAttributeNamesUseAMQPList(t *testing.T) {
	values := wireStringList([]string{"name", "identity"})
	if got, want := values, []any{"name", "identity"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wire attribute names = %#v, want %#v", got, want)
	}
}

func TestParseStatusError(t *testing.T) {
	message := &amqp.Message{
		ApplicationProperties: map[string]any{
			"statusCode":        uint32(404),
			"statusDescription": "listener not found",
		},
	}
	_, err := parseResponse(message, &Request{
		Operation: "READ",
		Type:      "io.skupper.router.listener",
		Ref:       ByName("missing"),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 404 || status.Ref.Name != "missing" {
		t.Fatalf("error = %#v", err)
	}
}

func TestDecodeRowsAndNumericVariants(t *testing.T) {
	body := map[any]any{
		"attributeNames": []any{"name", "count", "enabled"},
		"results": []any{
			[]any{"one", int32(7), true},
			[]any{"two", uint16(9), false},
		},
	}
	rows, err := decodeRows(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []testEntity
	fields, err := rowFields[testEntity](testEntityType, rows.Columns)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows.Values {
		var entity testEntity
		if err := decodeRow(&entity, fields, row); err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, entity)
	}
	want := []testEntity{{Name: "one", Count: 7, Enabled: true}, {Name: "two", Count: 9}}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded = %#v, want %#v", decoded, want)
	}
}

func BenchmarkDecodePositionalQuery1000Rows(b *testing.B) {
	results := make([]any, 1000)
	for i := range results {
		results[i] = []any{"listener", int32(i), i%2 == 0}
	}
	body := map[string]any{
		"attributeNames": []any{"name", "count", "enabled"},
		"results":        results,
	}
	rows, err := decodeRows(body)
	if err != nil {
		b.Fatal(err)
	}
	fields, err := rowFields[testEntity](testEntityType, rows.Columns)
	if err != nil {
		b.Fatal(err)
	}
	decoded := make([]testEntity, len(rows.Values))
	b.ReportAllocs()
	b.ReportMetric(float64(len(results)), "rows/op")
	b.ResetTimer()
	for b.Loop() {
		for i, row := range rows.Values {
			if err := decodeRow(&decoded[i], fields, row); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func TestTypedValidationHappensBeforeDial(t *testing.T) {
	dialed := false
	client := NewClient(func(context.Context) (*amqp.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	})

	_, err := Create[testEntity](context.Background(), client.Local(), Partial[testEntity]{
		Fields: Fields(testCount),
	})
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Op != "CREATE" {
		t.Fatalf("Create error = %#v", err)
	}
	if dialed {
		t.Fatal("invalid create dialed the transport")
	}

	_, err = Query[testEntity](context.Background(), client.Local(), 0)
	if !errors.As(err, &validation) || validation.Op != "QUERY/READ" {
		t.Fatalf("empty Query error = %#v", err)
	}
	if dialed {
		t.Fatal("empty query dialed the transport")
	}

	_, err = Query[testEntity](context.Background(), client.Local(), Fields(testName), Page(0, 10))
	if !errors.As(err, &validation) || validation.Op != "QUERY" {
		t.Fatalf("paged Query error = %#v", err)
	}
	if dialed {
		t.Fatal("invalid paged query dialed the transport")
	}
}

func TestClientIsLazyAndDialFailuresAreRetryable(t *testing.T) {
	dials := 0
	client := NewClient(func(context.Context) (*amqp.Conn, error) {
		dials++
		return nil, errors.New("connection refused")
	})
	if dials != 0 {
		t.Fatal("NewClient dialed eagerly")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := client.Local().Do(context.Background(), &Request{Operation: "GET-MGMT-NODES"})
		var transport *TransportError
		if !errors.As(err, &transport) || !transport.Retryable() {
			t.Fatalf("attempt %d error = %#v", attempt, err)
		}
	}
	if dials != 2 {
		t.Fatalf("dial attempts = %d, want 2", dials)
	}
}

func TestRequestTimeoutDuringDialReturnsContextError(t *testing.T) {
	client := NewClient(func(ctx context.Context) (*amqp.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, WithRequestTimeout(time.Millisecond))

	_, err := client.Local().Do(context.Background(), &Request{Operation: "READ"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request error = %v, want context deadline exceeded", err)
	}
	var transport *TransportError
	if errors.As(err, &transport) {
		t.Fatalf("request context error was wrapped as a transport failure: %v", err)
	}
}

func TestConcurrentCallUsesSharedDialAfterFirstCallerTimesOut(t *testing.T) {
	var dials atomic.Int32
	dialStarted := make(chan context.Context, 1)
	releaseDial := make(chan struct{})
	dialFailure := errors.New("dial failed for test")
	client := NewClient(func(ctx context.Context) (*amqp.Conn, error) {
		dials.Add(1)
		dialStarted <- ctx
		<-releaseDial
		return nil, dialFailure
	}, WithRequestTimeout(time.Second))

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		_, err := client.getState(firstCtx)
		firstResult <- err
	}()
	sharedDialCtx := <-dialStarted

	secondWaiting := make(chan struct{}, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, err := client.getState(observedDoneContext{Context: context.Background(), observed: secondWaiting})
		secondResult <- err
	}()
	<-secondWaiting

	if err := <-firstResult; !errors.Is(err, context.DeadlineExceeded) {
		close(releaseDial)
		t.Fatalf("first caller error = %v, want context deadline exceeded", err)
	}
	if err := sharedDialCtx.Err(); err != nil {
		close(releaseDial)
		t.Fatalf("first caller canceled the shared dial: %v", err)
	}
	close(releaseDial)
	var transport *TransportError
	if err := <-secondResult; !errors.As(err, &transport) || !errors.Is(err, dialFailure) {
		t.Fatalf("second caller error = %v, want shared dial failure", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial attempts = %d, want 1", got)
	}
}

func TestCloseReturnsWhileDialIsBlocked(t *testing.T) {
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	client := NewClient(func(context.Context) (*amqp.Conn, error) {
		close(dialStarted)
		<-releaseDial
		return nil, errors.New("dial released after close")
	}, WithRequestTimeout(time.Second))

	requestResult := make(chan error, 1)
	go func() {
		_, err := client.getState(context.Background())
		requestResult <- err
	}()
	<-dialStarted

	closeResult := make(chan error, 1)
	go func() { closeResult <- client.Close(context.Background()) }()
	select {
	case err := <-closeResult:
		if err != nil {
			close(releaseDial)
			t.Fatalf("Close error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(releaseDial)
		<-closeResult
		t.Fatal("Close blocked on the dial attempt")
	}
	if err := <-requestResult; !errors.Is(err, ErrClosed) {
		close(releaseDial)
		t.Fatalf("blocked request error = %v, want ErrClosed", err)
	}
	close(releaseDial)
}

func TestCloseWithoutRequestDoesNotDial(t *testing.T) {
	dialed := false
	client := NewClient(func(context.Context) (*amqp.Conn, error) {
		dialed = true
		return nil, nil
	})
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dialed {
		t.Fatal("Close dialed an unused client")
	}
	_, err := client.Local().Do(context.Background(), &Request{Operation: "READ"})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("request after Close = %v, want ErrClosed", err)
	}
}
