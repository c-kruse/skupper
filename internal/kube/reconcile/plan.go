package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

type OperationID string

type Operation struct {
	ID           OperationID
	Kind         string
	Dependencies []OperationID
	// After orders effects without requiring their success. Diagnostics can
	// run last even when a prerequisite failed or its dependents were skipped.
	After []OperationID
	Run   func(context.Context) error
}

type Plan struct {
	Namespace  NamespaceIdentity
	Operations []Operation
}

type ResultState string

const (
	Succeeded         ResultState = "succeeded"
	Failed            ResultState = "failed"
	UnknownOutcome    ResultState = "unknown-outcome"
	SkippedDependency ResultState = "skipped-dependency"
	Superseded        ResultState = "superseded"
)

type OperationResult struct {
	ID    OperationID
	State ResultState
	Error error
}

type ExecutionReport struct {
	Namespace  NamespaceIdentity
	Results    []OperationResult
	RetryAfter time.Duration
}

func (r ExecutionReport) NeedsRetry() bool {
	for _, result := range r.Results {
		if result.State == Failed || result.State == UnknownOutcome || result.State == SkippedDependency || result.State == Superseded {
			return true
		}
	}
	return false
}

type ambiguousError struct{ error }

// Ambiguous marks an effect that may have succeeded despite its returned error.
// The next attempt must recollect instead of blindly replaying dependent work.
func Ambiguous(err error) error {
	if err == nil {
		return nil
	}
	return ambiguousError{error: err}
}

type SupersededError struct{ Reason string }

func (e SupersededError) Error() string { return "plan superseded: " + e.Reason }

type Executor struct{}

func (Executor) Execute(ctx context.Context, plan Plan) ExecutionReport {
	report := ExecutionReport{Namespace: plan.Namespace}
	states := map[OperationID]ResultState{}
	operations := append([]Operation(nil), plan.Operations...)
	sort.SliceStable(operations, func(i, j int) bool { return operations[i].ID < operations[j].ID })
	pending := make(map[OperationID]Operation, len(operations))
	for _, operation := range operations {
		if operation.ID == "" || operation.Run == nil {
			report.Results = append(report.Results, OperationResult{ID: operation.ID, State: Failed, Error: fmt.Errorf("invalid operation")})
			continue
		}
		if _, exists := pending[operation.ID]; exists {
			report.Results = append(report.Results, OperationResult{ID: operation.ID, State: Failed, Error: fmt.Errorf("duplicate operation ID")})
			continue
		}
		pending[operation.ID] = operation
	}
	for len(pending) > 0 {
		progress := false
		for _, operation := range operations {
			if _, ok := pending[operation.ID]; !ok {
				continue
			}
			ready := true
			blocked := false
			for _, dependency := range operation.Dependencies {
				state, completed := states[dependency]
				if !completed {
					if _, exists := pending[dependency]; exists {
						ready = false
						continue
					}
					blocked = true
					continue
				}
				if state != Succeeded {
					blocked = true
				}
			}
			for _, predecessor := range operation.After {
				if _, completed := states[predecessor]; !completed {
					if _, exists := pending[predecessor]; exists {
						ready = false
					} else {
						blocked = true
					}
				}
			}
			if !ready {
				continue
			}
			result := OperationResult{ID: operation.ID}
			if blocked {
				result.State = SkippedDependency
			} else if err := ctx.Err(); err != nil {
				result.State = Failed
				result.Error = err
			} else if err := operation.Run(ctx); err != nil {
				var superseded SupersededError
				var ambiguous ambiguousError
				switch {
				case errors.As(err, &superseded):
					result.State = Superseded
				case errors.As(err, &ambiguous):
					result.State = UnknownOutcome
				default:
					result.State = Failed
				}
				result.Error = err
			} else {
				result.State = Succeeded
			}
			states[operation.ID] = result.State
			report.Results = append(report.Results, result)
			delete(pending, operation.ID)
			progress = true
		}
		if progress {
			continue
		}
		for _, operation := range operations {
			if _, ok := pending[operation.ID]; ok {
				report.Results = append(report.Results, OperationResult{ID: operation.ID, State: Failed, Error: fmt.Errorf("dependency cycle")})
				delete(pending, operation.ID)
			}
		}
	}
	return report
}
