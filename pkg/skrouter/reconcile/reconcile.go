// Package reconcile synchronizes one named management entity type at a time.
// Callers are responsible for ordering operations across dependent entity types.
package reconcile

import (
	"context"
	"fmt"

	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
)

// Entity adds generated comparison and defaulting methods to a management entity.
type Entity[E any] interface {
	mgmt.Entity[E]
	ApplyDefaults(mgmt.FieldSet[E])
	Diff(*E, mgmt.FieldSet[E]) mgmt.FieldSet[E]
}

// Options controls comparison and removal of entities.
type Options[E any] struct {
	// Owned authorizes deletion of actual entities absent from desired. Nil
	// preserves all extras. Desired names explicitly authorize updates/replacement.
	// When set, the query fetches all fields so the predicate sees complete values.
	Owned func(E) bool
	// Canonicalize normalizes desired values to the router's reported form.
	// It must not change the name or mutate maps/slices shared with the caller.
	Canonicalize func(*E)
	// Compare overrides the per-item desired field mask and must contain only
	// createable fields. Omitted values in this mask use schema defaults. Zero
	// compares only explicitly desired fields.
	Compare mgmt.FieldSet[E]
	// Precheck skips a desired entry on error, preserving any existing entry of
	// that name. It receives the defaulted, canonicalized desired value.
	Precheck func(mgmt.Partial[E]) error
}

// Report counts completed actions. Replaced is a successful delete/create pair,
// not also a Created or Deleted action. On error the report may be partial.
type Report struct {
	Created, Updated, Replaced, Deleted, Unchanged int
	Skipped                                        []error
}

// Sync queries once, then creates, updates exactly differing mutable fields,
// replaces immutable differences, and finally deletes owned extras. Management
// errors stop the sync. Replacement is not atomic: a failed create leaves the
// old entity deleted. Unspecified desired fields are not managed by default.
func Sync[E any, PE Entity[E]](ctx context.Context, target mgmt.Target, desired []mgmt.Partial[E], options Options[E]) (Report, error) {
	return syncEntities[E, PE](ctx, desired, options, operations[E]{
		query:  func(fields mgmt.FieldSet[E]) ([]E, error) { return mgmt.Query[E, PE](ctx, target, fields) },
		create: func(p mgmt.Partial[E]) error { _, err := mgmt.Create[E, PE](ctx, target, p); return err },
		update: func(ref mgmt.Ref, p mgmt.Partial[E]) error {
			_, err := mgmt.Update[E, PE](ctx, target, ref, p)
			return err
		},
		delete: func(ref mgmt.Ref) error { return mgmt.Delete[E, PE](ctx, target, ref) },
	})
}

// operations keeps the reconciliation algorithm independently testable without
// introducing an alternative public management transport API.
type operations[E any] struct {
	query  func(mgmt.FieldSet[E]) ([]E, error)
	create func(mgmt.Partial[E]) error
	update func(mgmt.Ref, mgmt.Partial[E]) error
	delete func(mgmt.Ref) error
}

func syncEntities[E any, PE Entity[E]](ctx context.Context, desired []mgmt.Partial[E], options Options[E], ops operations[E]) (Report, error) {
	var report Report
	typ := mgmt.TypeOf[E, PE]()
	var all, create, update mgmt.FieldSet[E]
	var name, identity mgmt.Field[E]
	var hasName, hasIdentity bool
	for i, attr := range typ.Attributes {
		field := mgmt.Field[E](i)
		bit := mgmt.Fields(field)
		all |= bit
		if attr.Create {
			create |= bit
		}
		if attr.Update {
			update |= bit
		}
		if attr.Name == "name" {
			name, hasName = field, true
		}
		if attr.Name == "identity" {
			identity, hasIdentity = field, true
		}
	}
	if !hasName || !typ.Supports("QUERY") {
		return report, fmt.Errorf("reconcile %s: requires named, queryable entities", typ.Short)
	}
	if options.Compare.Without(create) != 0 {
		return report, fmt.Errorf("reconcile %s: invalid comparison fields", typ.Short)
	}
	getName := func(value *E) string { s, _ := mgmt.AsString(PE(value).ManagementValue(name)); return s }
	refFor := func(value *E) mgmt.Ref {
		if hasIdentity {
			id, _ := mgmt.AsString(PE(value).ManagementValue(identity))
			if id != "" {
				return mgmt.ByIdentity(id)
			}
		}
		return mgmt.ByName(getName(value))
	}
	projection := mgmt.Fields(name) | options.Compare
	if hasIdentity {
		projection |= mgmt.Fields(identity)
	}
	prepared := append([]mgmt.Partial[E](nil), desired...)
	names := make(map[string]bool, len(desired))
	skipped := make([]bool, len(desired))
	for i := range prepared {
		p := &prepared[i]
		n := getName(&p.Value)
		if !p.Fields.Has(name) || n == "" || names[n] {
			return report, fmt.Errorf("reconcile %s: missing or duplicate desired name %q", typ.Short, n)
		}
		names[n] = true
		if p.Fields.Without(create) != 0 {
			return report, fmt.Errorf("reconcile %s %q: desired fields are not createable", typ.Short, n)
		}
		PE(&p.Value).ApplyDefaults(p.Fields)
		if options.Canonicalize != nil {
			options.Canonicalize(&p.Value)
		}
		if getName(&p.Value) != n {
			return report, fmt.Errorf("reconcile %s %q: canonicalization changed name", typ.Short, n)
		}
		if options.Precheck != nil {
			if err := options.Precheck(*p); err != nil {
				skipped[i] = true
				report.Skipped = append(report.Skipped, fmt.Errorf("reconcile %s %q: %w", typ.Short, n, err))
			}
		}
		if options.Compare == 0 {
			projection |= p.Fields
		}
	}
	if options.Owned != nil {
		projection = all
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	actual, err := ops.query(projection)
	if err != nil {
		return report, fmt.Errorf("reconcile %s query: %w", typ.Short, err)
	}
	byName := make(map[string]int, len(actual))
	for i := range actual {
		n := getName(&actual[i])
		if _, exists := byName[n]; exists || n == "" {
			return report, fmt.Errorf("reconcile %s: missing or duplicate actual name %q", typ.Short, n)
		}
		byName[n] = i
	}
	for i, p := range prepared {
		if skipped[i] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		n := getName(&p.Value)
		index, exists := byName[n]
		if !exists {
			if err := ops.create(p); err != nil {
				return report, fmt.Errorf("reconcile %s %q create: %w", typ.Short, n, err)
			}
			report.Created++
			continue
		}
		fields := options.Compare
		if fields == 0 {
			fields = p.Fields
		}
		diff := PE(&actual[index]).Diff(&p.Value, fields)
		if diff == 0 {
			report.Unchanged++
			continue
		}
		ref := refFor(&actual[index])
		if diff.Without(update) == 0 && typ.Supports("UPDATE") {
			if err := ops.update(ref, mgmt.Partial[E]{Value: p.Value, Fields: diff}); err != nil {
				return report, fmt.Errorf("reconcile %s %q update: %w", typ.Short, n, err)
			}
			report.Updated++
		} else {
			// Check both operations before deleting anything.
			if !typ.Supports("CREATE") || !typ.Supports("DELETE") {
				return report, fmt.Errorf("reconcile %s %q: replacement unsupported", typ.Short, n)
			}
			if err := ops.delete(ref); err != nil {
				return report, fmt.Errorf("reconcile %s %q replace/delete: %w", typ.Short, n, err)
			}
			if err := ops.create(p); err != nil {
				return report, fmt.Errorf("reconcile %s %q replace/create (old entity deleted): %w", typ.Short, n, err)
			}
			report.Replaced++
		}
	}
	for i := range actual {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		n := getName(&actual[i])
		if !names[n] && options.Owned != nil && options.Owned(actual[i]) {
			if err := ops.delete(refFor(&actual[i])); err != nil {
				return report, fmt.Errorf("reconcile %s %q delete: %w", typ.Short, n, err)
			}
			report.Deleted++
		}
	}
	return report, nil
}
