package index

import (
	"context"
	"fmt"

	"github.com/c-kruse/vanflow"
	"github.com/c-kruse/vanflow/store"
)

// Fetch is a generic helper to get raw records out of a store index
func Fetch[R vanflow.Record](ctx context.Context, stor store.Interface, index string, exemplar R) ([]R, error) {
	entry := store.Entry{
		TypeMeta: exemplar.GetTypeMeta(),
		Record:   exemplar,
	}
	resultSet, err := stor.Index(ctx, index, entry, nil)
	if err != nil {
		return nil, err
	}
	results := make([]R, len(resultSet.Entries))
	for i, entry := range resultSet.Entries {
		record, ok := entry.Record.(R)
		if !ok {
			return results, fmt.Errorf("unexpected record type in store: %T", entry.Record)
		}
		results[i] = record
	}
	return results, nil
}
