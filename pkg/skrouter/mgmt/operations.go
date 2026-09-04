package mgmt

import (
	"context"
	"fmt"
	"iter"
	"slices"
)

type queryOptions struct {
	paged  bool
	offset int
	count  int
}

// QueryOption configures a QUERY request.
type QueryOption func(*queryOptions) error

// Page requests a page of entities from a core-agent type.
func Page(offset, count int) QueryOption {
	return func(options *queryOptions) error {
		if offset < 0 || count <= 0 {
			return fmt.Errorf("management page requires offset >= 0 and count > 0")
		}
		options.paged = true
		options.offset = offset
		options.count = count
		return nil
	}
}

// Rows is the positional result of a raw QUERY.
type Rows struct {
	Columns []string
	Values  [][]any
}

// QueryRaw queries an entity type without generated entity metadata.
func QueryRaw(ctx context.Context, target Target, typeName string, attributes []string, options ...QueryOption) (Rows, error) {
	query, err := applyQueryOptions(options)
	if err != nil {
		return Rows{}, err
	}
	request := &Request{
		Operation: "QUERY",
		Type:      typeName,
		Body:      map[string]any{"attributeNames": wireStringList(attributes)},
	}
	if query.paged {
		request.Properties = map[string]any{
			"offset": int64(query.offset),
			"count":  int64(query.count),
		}
	}
	response, err := target.Do(ctx, request)
	if err != nil {
		return Rows{}, err
	}
	return decodeRows(response.Body)
}

// Query returns typed entities decoded directly from positional query rows.
// Fields must contain at least one attribute; use the generated AllFields set
// to request every attribute.
func Query[E any, PE Entity[E]](ctx context.Context, target Target, fields FieldSet[E], options ...QueryOption) ([]E, error) {
	return QueryAppend[E, PE](ctx, target, fields, nil, options...)
}

// QueryAppend appends typed query results to dst.
func QueryAppend[E any, PE Entity[E]](ctx context.Context, target Target, fields FieldSet[E], dst []E, options ...QueryOption) ([]E, error) {
	rows, typ, err := queryRows[E, PE](ctx, target, fields, options)
	if err != nil {
		return dst, err
	}
	ordinals, err := rowFields[E](typ, rows.Columns)
	if err != nil {
		return dst, err
	}
	start := len(dst)
	dst = slices.Grow(dst, len(rows.Values))
	dst = dst[:start+len(rows.Values)]
	for rowIndex, row := range rows.Values {
		if err := decodeRow(PE(&dst[start+rowIndex]), ordinals, row); err != nil {
			return dst[:start], fmt.Errorf("decode %s query row %d: %w", typ.Name, rowIndex, err)
		}
	}
	return dst, nil
}

// QueryEach invokes yield once for every typed query row.
func QueryEach[E any, PE Entity[E]](ctx context.Context, target Target, fields FieldSet[E], yield func(*E) error, options ...QueryOption) error {
	if yield == nil {
		return fmt.Errorf("management query callback is nil")
	}
	rows, typ, err := queryRows[E, PE](ctx, target, fields, options)
	if err != nil {
		return err
	}
	ordinals, err := rowFields[E](typ, rows.Columns)
	if err != nil {
		return err
	}
	for rowIndex, row := range rows.Values {
		var entity E
		if err := decodeRow(PE(&entity), ordinals, row); err != nil {
			return fmt.Errorf("decode %s query row %d: %w", typ.Name, rowIndex, err)
		}
		if err := yield(&entity); err != nil {
			return err
		}
	}
	return nil
}

func queryRows[E any, PE Entity[E]](ctx context.Context, target Target, fields FieldSet[E], options []QueryOption) (Rows, *Type, error) {
	typ := TypeOf[E, PE]()
	fields, err := checkedFields(fields, typ)
	if err != nil {
		return Rows{}, nil, err
	}
	query, err := applyQueryOptions(options)
	if err != nil {
		return Rows{}, nil, err
	}
	if query.paged && !typ.Core {
		return Rows{}, nil, &ValidationError{Type: typ.Name, Fields: []string{"offset", "count"}, Op: "QUERY"}
	}
	request := &Request{
		Operation: "QUERY",
		Type:      typ.Name,
		Body:      map[string]any{"attributeNames": wireStringList(typ.names(uint64(fields)))},
	}
	if query.paged {
		request.Properties = map[string]any{
			"offset": int64(query.offset),
			"count":  int64(query.count),
		}
	}
	response, err := target.Do(ctx, request)
	if err != nil {
		return Rows{}, nil, err
	}
	rows, err := decodeRows(response.Body)
	return rows, typ, err
}

// Read returns one entity. Although the router returns every attribute, only
// fields is decoded into the result. Fields must contain at least one
// attribute; use the generated AllFields set to decode every attribute.
func Read[E any, PE Entity[E]](ctx context.Context, target Target, ref Ref, fields FieldSet[E]) (E, error) {
	var result E
	if err := ref.validate(); err != nil {
		return result, err
	}
	typ := TypeOf[E, PE]()
	fields, err := checkedFields(fields, typ)
	if err != nil {
		return result, err
	}
	response, err := target.Do(ctx, &Request{Operation: "READ", Type: typ.Name, Ref: ref})
	if err != nil {
		return result, err
	}
	if err := decodeMap(PE(&result), typ, fields, response.Body); err != nil {
		return result, fmt.Errorf("decode %s READ: %w", typ.Name, err)
	}
	return result, nil
}

// Create creates an entity using exactly the fields present in partial.
func Create[E any, PE Entity[E]](ctx context.Context, target Target, partial Partial[E]) (E, error) {
	return mutate[E, PE](ctx, target, "CREATE", Ref{}, partial)
}

// Update updates exactly the fields present in partial.
func Update[E any, PE Entity[E]](ctx context.Context, target Target, ref Ref, partial Partial[E]) (E, error) {
	var zero E
	if err := ref.validate(); err != nil {
		return zero, err
	}
	return mutate[E, PE](ctx, target, "UPDATE", ref, partial)
}

func mutate[E any, PE Entity[E]](ctx context.Context, target Target, operation string, ref Ref, partial Partial[E]) (E, error) {
	var result E
	typ := TypeOf[E, PE]()
	if !typ.Supports(operation) {
		return result, &ValidationError{Type: typ.Name, Op: operation}
	}
	allowed := typ.createFields
	if operation == "UPDATE" {
		allowed = typ.updateFields
	}
	invalid := uint64(partial.Fields) &^ allowed
	if invalid != 0 {
		return result, &ValidationError{Type: typ.Name, Fields: typ.names(invalid), Op: operation}
	}
	body := make(map[string]any)
	for name, value := range Attributes[E, PE](partial) {
		body[name] = value
	}
	if operation == "CREATE" {
		if ordinal, found := typ.byName["name"]; found {
			field := Field[E](ordinal)
			if partial.Fields.Has(field) {
				ref.Name, _ = AsString(PE(&partial.Value).ManagementValue(field))
			}
		}
	}
	response, err := target.Do(ctx, &Request{Operation: operation, Type: typ.Name, Ref: ref, Body: body})
	if err != nil {
		return result, err
	}
	if err := decodeMap(PE(&result), typ, FieldSet[E](typ.allFields), response.Body); err != nil {
		return result, fmt.Errorf("decode %s %s: %w", typ.Name, operation, err)
	}
	return result, nil
}

// Delete deletes an entity.
func Delete[E any, PE Entity[E]](ctx context.Context, target Target, ref Ref) error {
	if err := ref.validate(); err != nil {
		return err
	}
	typ := TypeOf[E, PE]()
	if !typ.Supports("DELETE") {
		return &ValidationError{Type: typ.Name, Op: "DELETE"}
	}
	_, err := target.Do(ctx, &Request{Operation: "DELETE", Type: typ.Name, Ref: ref})
	return err
}

// Attributes yields present fields in schema order.
func Attributes[E any, PE Entity[E]](partial Partial[E]) iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		typ := TypeOf[E, PE]()
		entity := PE(&partial.Value)
		for ordinal, attribute := range typ.Attributes {
			field := Field[E](ordinal)
			if partial.Fields.Has(field) && !yield(attribute.Name, entity.ManagementValue(field)) {
				return
			}
		}
	}
}

func checkedFields[E any](fields FieldSet[E], typ *Type) (FieldSet[E], error) {
	if fields == 0 {
		return 0, &ValidationError{Type: typ.Name, Fields: []string{"<empty field set>"}, Op: "QUERY/READ"}
	}
	invalid := uint64(fields) &^ typ.allFields
	if invalid != 0 {
		return 0, &ValidationError{Type: typ.Name, Fields: typ.names(invalid), Op: "QUERY/READ"}
	}
	return fields, nil
}

func applyQueryOptions(options []QueryOption) (queryOptions, error) {
	var result queryOptions
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&result); err != nil {
			return queryOptions{}, err
		}
	}
	return result, nil
}

func wireStringList(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func decodeRows(body any) (Rows, error) {
	mapped, ok := asMap(body, false)
	if !ok {
		return Rows{}, fmt.Errorf("management QUERY body is %T, want map", body)
	}
	columns, ok := AsStringList(mapped["attributeNames"])
	if !ok {
		return Rows{}, fmt.Errorf("management QUERY attributeNames is %T, want string list", mapped["attributeNames"])
	}
	values, ok := mapped["results"].([]any)
	if !ok {
		if typed, typedOK := mapped["results"].([][]any); typedOK {
			return Rows{Columns: columns, Values: typed}, nil
		}
		return Rows{}, fmt.Errorf("management QUERY results is %T, want row list", mapped["results"])
	}
	rows := make([][]any, len(values))
	for i, value := range values {
		row, ok := value.([]any)
		if !ok {
			return Rows{}, fmt.Errorf("management QUERY row %d is %T, want list", i, value)
		}
		rows[i] = row
	}
	return Rows{Columns: columns, Values: rows}, nil
}

func rowFields[E any](typ *Type, columns []string) ([]Field[E], error) {
	result := make([]Field[E], len(columns))
	for i, name := range columns {
		ordinal, ok := typ.byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown %s attribute %q", typ.Name, name)
		}
		result[i] = Field[E](ordinal)
	}
	return result, nil
}

func decodeRow[E any, PE Entity[E]](entity PE, fields []Field[E], row []any) error {
	if len(fields) != len(row) {
		return fmt.Errorf("got %d values for %d columns", len(row), len(fields))
	}
	for i, field := range fields {
		if err := entity.SetManagementField(field, row[i]); err != nil {
			return err
		}
	}
	return nil
}

func decodeMap[E any, PE Entity[E]](entity PE, typ *Type, fields FieldSet[E], body any) error {
	values, ok := AsMap(body)
	if !ok {
		return fmt.Errorf("response body is %T, want map", body)
	}
	for ordinal, attribute := range typ.Attributes {
		field := Field[E](ordinal)
		if !fields.Has(field) {
			continue
		}
		value, present := values[attribute.Name]
		if !present {
			continue
		}
		if err := entity.SetManagementField(field, value); err != nil {
			return err
		}
	}
	return nil
}
