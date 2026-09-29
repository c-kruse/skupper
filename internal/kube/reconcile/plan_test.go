package reconcile

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestExecutorRetainsIndependentSuccessAndBlocksDependents(t *testing.T) {
	var ran []string
	plan := Plan{Operations: []Operation{
		{ID: "publish", Dependencies: []OperationID{"allocation"}, Run: func(context.Context) error { ran = append(ran, "publish"); return nil }},
		{ID: "status", Run: func(context.Context) error { ran = append(ran, "status"); return nil }},
		{ID: "allocation", Run: func(context.Context) error {
			ran = append(ran, "allocation")
			return Ambiguous(errors.New("timeout after write"))
		}},
	}}
	report := (Executor{}).Execute(context.Background(), plan)
	if !reflect.DeepEqual(ran, []string{"allocation", "status"}) {
		t.Fatalf("unexpected operations ran: %v", ran)
	}
	states := map[OperationID]ResultState{}
	for _, result := range report.Results {
		states[result.ID] = result.State
	}
	if states["allocation"] != UnknownOutcome || states["publish"] != SkippedDependency || states["status"] != Succeeded {
		t.Fatalf("unexpected result states: %#v", states)
	}
}
