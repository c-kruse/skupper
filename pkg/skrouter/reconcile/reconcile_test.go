package reconcile

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt/entities"
)

type recording struct {
	actual  []entities.TcpListener
	queries []mgmt.FieldSet[entities.TcpListener]
	actions []string
	patches []mgmt.Partial[entities.TcpListener]
	fail    string
}

var failure = errors.New("management failed")

func (r *recording) operations() operations[entities.TcpListener] {
	return operations[entities.TcpListener]{
		query: func(fields mgmt.FieldSet[entities.TcpListener]) ([]entities.TcpListener, error) {
			r.queries = append(r.queries, fields)
			if r.fail == "query" {
				return nil, failure
			}
			// Honor the projection, catching accidental dependencies on unqueried fields.
			result := make([]entities.TcpListener, len(r.actual))
			for i := range r.actual {
				for name, value := range mgmt.Attributes[entities.TcpListener](mgmt.Partial[entities.TcpListener]{Value: r.actual[i], Fields: fields}) {
					for j, attr := range entities.TcpListenerType.Attributes {
						if attr.Name == name {
							_ = result[i].SetManagementField(mgmt.Field[entities.TcpListener](j), value)
						}
					}
				}
			}
			return result, nil
		},
		create: func(p mgmt.Partial[entities.TcpListener]) error {
			r.actions = append(r.actions, "create "+p.Value.Name)
			r.patches = append(r.patches, p)
			if r.fail == "create" {
				return failure
			}
			return nil
		},
		update: func(ref mgmt.Ref, p mgmt.Partial[entities.TcpListener]) error {
			r.actions = append(r.actions, "update "+ref.String())
			r.patches = append(r.patches, p)
			if r.fail == "update" {
				return failure
			}
			return nil
		},
		delete: func(ref mgmt.Ref) error {
			r.actions = append(r.actions, "delete "+ref.String())
			if r.fail == "delete" {
				return failure
			}
			return nil
		},
	}
}

func listener(name, port string) mgmt.Partial[entities.TcpListener] {
	return mgmt.Partial[entities.TcpListener]{Value: entities.TcpListener{Name: name, Port: port}, Fields: mgmt.Fields(entities.TcpListenerName, entities.TcpListenerPort)}
}

func TestActions(t *testing.T) {
	change := listener("update", "8080")
	change.Value.Observer = entities.Observer("none")
	change.Fields |= mgmt.Fields(entities.TcpListenerObserver)
	r := recording{actual: []entities.TcpListener{
		{Name: "same", Port: "8080"},
		{Name: "update", Identity: "42", Port: "8080", Observer: entities.Observer("auto")},
		{Name: "replace", Identity: "43", Port: "8081"},
		{Name: "extra", Identity: "44", SiteId: "ours"},
		{Name: "unowned", SiteId: "theirs"},
	}}
	report, err := syncEntities[entities.TcpListener](context.Background(), []mgmt.Partial[entities.TcpListener]{listener("same", "8080"), listener("new", "8080"), change, listener("replace", "8080")}, Options[entities.TcpListener]{Owned: func(e entities.TcpListener) bool { return e.SiteId == "ours" }}, r.operations())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report, Report{Created: 1, Updated: 1, Replaced: 1, Deleted: 1, Unchanged: 1}) {
		t.Fatal(report)
	}
	want := []string{"create new", "update identity=42", "delete identity=43", "create replace", "delete identity=44"}
	if !reflect.DeepEqual(r.actions, want) {
		t.Fatalf("actions %v, want %v", r.actions, want)
	}
	if len(r.queries) != 1 || r.queries[0] != entities.TcpListenerAllFields {
		t.Fatalf("ownership projection: %v", r.queries)
	}
	if r.patches[1].Fields != mgmt.Fields(entities.TcpListenerObserver) {
		t.Fatalf("update includes unchanged fields: %v", r.patches[1].Fields)
	}
}

func TestProjectionDefaultsAndCanonicalization(t *testing.T) {
	p := listener("one", "0")
	p.Value.Host = "0.0.0.0"
	p.Fields |= mgmt.Fields(entities.TcpListenerHost)
	original := p
	r := recording{actual: []entities.TcpListener{{Name: "one", Port: "0", Host: "", Observer: entities.Observer("auto")}, {Name: "extra"}}}
	compare := p.Fields | mgmt.Fields(entities.TcpListenerObserver)
	report, err := syncEntities[entities.TcpListener](context.Background(), []mgmt.Partial[entities.TcpListener]{p}, Options[entities.TcpListener]{Compare: compare, Canonicalize: func(e *entities.TcpListener) {
		if e.Host == "0.0.0.0" {
			e.Host = ""
		}
	}}, r.operations())
	if err != nil || report.Unchanged != 1 || len(r.actions) != 0 {
		t.Fatalf("%+v %v %v", report, err, r.actions)
	}
	if r.queries[0] != compare|mgmt.Fields(entities.TcpListenerIdentity) {
		t.Fatal(r.queries)
	}
	if !reflect.DeepEqual(p, original) {
		t.Fatal("mutated desired")
	}
}

func TestSkippedDesiredIsNotDeleted(t *testing.T) {
	r := recording{actual: []entities.TcpListener{{Name: "skip", Port: "old"}, {Name: "extra"}}}
	report, err := syncEntities[entities.TcpListener](context.Background(), []mgmt.Partial[entities.TcpListener]{listener("skip", "new")}, Options[entities.TcpListener]{Owned: func(entities.TcpListener) bool { return true }, Precheck: func(mgmt.Partial[entities.TcpListener]) error { return failure }}, r.operations())
	if err != nil || report.Deleted != 1 || len(report.Skipped) != 1 || !errors.Is(report.Skipped[0], failure) {
		t.Fatalf("%+v %v", report, err)
	}
	if !reflect.DeepEqual(r.actions, []string{"delete name=extra"}) {
		t.Fatal(r.actions)
	}
}

func TestInvalidDesiredDoesNotQueryOrMutate(t *testing.T) {
	badMask := listener("bad", "1")
	badMask.Fields |= 1 << 63
	readOnly := listener("read-only", "1")
	readOnly.Fields |= mgmt.Fields(entities.TcpListenerBytesIn)
	for _, desired := range [][]mgmt.Partial[entities.TcpListener]{
		{listener("", "1")}, {listener("dup", "1"), listener("dup", "2")}, {listener("good", "1"), badMask}, {readOnly},
	} {
		r := recording{}
		_, err := syncEntities[entities.TcpListener](context.Background(), desired, Options[entities.TcpListener]{}, r.operations())
		if err == nil || len(r.queries) != 0 || len(r.actions) != 0 {
			t.Fatalf("%v: %v %+v", desired, err, r)
		}
	}
}

func TestFailuresAndPartialReport(t *testing.T) {
	for _, fail := range []string{"query", "delete", "create", "update"} {
		t.Run(fail, func(t *testing.T) {
			r := recording{actual: []entities.TcpListener{{Name: "same", Port: "1"}, {Name: "change", Port: "old", Observer: entities.Observer("auto")}}, fail: fail}
			p := listener("change", "new")
			if fail == "update" {
				p.Value.Port = "old"
				p.Value.Observer = entities.Observer("none")
				p.Fields |= mgmt.Fields(entities.TcpListenerObserver)
			}
			report, err := syncEntities[entities.TcpListener](context.Background(), []mgmt.Partial[entities.TcpListener]{listener("same", "1"), p}, Options[entities.TcpListener]{}, r.operations())
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if report.Replaced != 0 || (fail != "query" && report.Unchanged != 1) {
				t.Fatal(report)
			}
			if fail == "create" && !strings.Contains(err.Error(), "old entity deleted") {
				t.Fatal(err)
			}
			if fail == "delete" && len(r.actions) != 1 {
				t.Fatal(r.actions)
			}
		})
	}
}

func TestCancellationAndActualDuplicates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := recording{}
	_, err := syncEntities[entities.TcpListener](ctx, nil, Options[entities.TcpListener]{}, r.operations())
	if !errors.Is(err, context.Canceled) || len(r.queries) != 0 {
		t.Fatal(err, r.queries)
	}
	r.actual = []entities.TcpListener{{Name: "duplicate"}, {Name: "duplicate"}}
	_, err = syncEntities[entities.TcpListener](context.Background(), nil, Options[entities.TcpListener]{Owned: func(entities.TcpListener) bool { return true }}, r.operations())
	if err == nil || len(r.actions) != 0 {
		t.Fatal(err, r.actions)
	}
}

func TestSyncUsesManagementTarget(t *testing.T) {
	_, err := Sync[entities.TcpListener](context.Background(), mgmt.Target{}, nil, Options[entities.TcpListener]{})
	if err == nil || !strings.Contains(err.Error(), "management target has no client") {
		t.Fatal(err)
	}
}

func TestDefaultComparisonUsesEachDesiredMask(t *testing.T) {
	first := listener("first", "1")
	second := listener("second", "2")
	second.Fields |= mgmt.Fields(entities.TcpListenerAuthenticatePeer)
	r := recording{actual: []entities.TcpListener{
		{Name: "first", Port: "1", AuthenticatePeer: true, Host: "unmanaged"},
		{Name: "second", Port: "2", AuthenticatePeer: true},
	}}
	report, err := syncEntities[entities.TcpListener](context.Background(), []mgmt.Partial[entities.TcpListener]{first, second}, Options[entities.TcpListener]{}, r.operations())
	if err != nil || report.Unchanged != 1 || report.Replaced != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	want := first.Fields | second.Fields | mgmt.Fields(entities.TcpListenerIdentity)
	if len(r.queries) != 1 || r.queries[0] != want {
		t.Fatal(r.queries)
	}
	if len(r.patches) != 1 || r.patches[0].Fields != second.Fields || r.patches[0].Value.AuthenticatePeer {
		t.Fatal(r.patches)
	}
}

func TestInvalidOptions(t *testing.T) {
	for _, opts := range []Options[entities.TcpListener]{
		{Compare: 1 << 63},
		{Compare: mgmt.Fields(entities.TcpListenerBytesIn)},
		{Canonicalize: func(e *entities.TcpListener) { e.Name = "different" }},
	} {
		r := recording{}
		_, err := syncEntities[entities.TcpListener](context.Background(), []mgmt.Partial[entities.TcpListener]{listener("test", "1")}, opts, r.operations())
		if err == nil || len(r.queries) != 0 {
			t.Fatal(err, r.queries)
		}
	}
}
