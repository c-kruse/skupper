package store

import (
	"fmt"
	"time"

	v1 "github.com/skupperproject/skupper/pkg/flow/v1"
	"github.com/skupperproject/skupper/pkg/flow/v1/encoding"
)

var errNoChange = fmt.Errorf("resource not changed")

// recordPatcher uses flow/v1/encoding to compare records as attribute sets
type recordPatcher struct {
	Record v1.Record
	Source SourceRef
}

func (p recordPatcher) Patch(curr *Entry) (next Entry, err error) {
	if curr == nil {
		return Entry{
			Record: p.Record,
			Meta: Metadata{
				UpdatedAt: time.Now(),
				Source:    p.Source,
			},
			TypeMeta: p.Record.GetTypeMeta(),
		}, nil
	}
	entry := *curr
	currAttrs, err := encoding.Encode(entry.Record)
	if err != nil {
		return next, fmt.Errorf("error encoding current record for comparison: %w", err)
	}
	nextAttrs, err := encoding.Encode(p.Record)
	if err != nil {
		return next, fmt.Errorf("error encoding incoming record for comparison: %w", err)
	}
	var changed bool
	for nK, nV := range nextAttrs {
		cV, ok := currAttrs[nK]
		if !ok || cV != nV {
			changed = true
			currAttrs[nK] = nV
		}
	}
	if !changed {
		return next, errNoChange
	}
	entry.Meta.UpdatedAt = time.Now()
	rawOut, err := encoding.Decode(currAttrs)
	entry.Record = rawOut.(v1.Record)
	return entry, err
}
