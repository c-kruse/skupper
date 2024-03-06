package queue

import (
	"fmt"
	"log/slog"

	"github.com/c-kruse/vanflow/store"
)

type Queue[R any] struct {
	Events chan Event[R]
}

type Action string

const (
	Changed Action = "CHANGED"
	Deleted Action = "DELETED"
)

type Event[R any] struct {
	Action Action
	Record R
}

func New[R any]() Queue[R] {
	return Queue[R]{
		Events: make(chan Event[R], 256),
	}
}

func (q Queue[R]) Handler() store.EventHandlerFuncs {
	return store.EventHandlerFuncs{
		OnAdd:    q.onAdd,
		OnChange: q.onChange,
		OnDelete: q.onDelete,
	}
}

func (q Queue[R]) onAdd(e store.Entry) {
	record, ok := e.Record.(R)
	if !ok {
		var exp R
		slog.Error("unexpected type added to queue", slog.String("type", fmt.Sprintf("%T", e.Record)), slog.String("expected", fmt.Sprintf("%T", exp)))
		return
	}
	q.Events <- Event[R]{Action: Changed, Record: record}
}

func (q Queue[R]) onChange(_, e store.Entry) {
	record, ok := e.Record.(R)
	if !ok {
		var exp R
		slog.Error("unexpected type added to queue", slog.String("type", fmt.Sprintf("%T", e.Record)), slog.String("expected", fmt.Sprintf("%T", exp)))
		return
	}
	q.Events <- Event[R]{Action: Changed, Record: record}
}

func (q Queue[R]) onDelete(e store.Entry) {
	record, ok := e.Record.(R)
	if !ok {
		var exp R
		slog.Error("unexpected type added to queue", slog.String("type", fmt.Sprintf("%T", e.Record)), slog.String("expected", fmt.Sprintf("%T", exp)))
		return
	}
	q.Events <- Event[R]{Action: Deleted, Record: record}
}
