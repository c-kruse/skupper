package mgmt

import (
	"fmt"
	"math"
	"reflect"
)

// AsString converts the string representation used by generated decoders.
func AsString(value any) (string, bool) {
	if value == nil {
		return "", true
	}
	result, ok := value.(string)
	return result, ok
}

// AsInt64 accepts all AMQP integer widths that fit in an int64.
func AsInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case nil:
		return 0, true
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		if uint64(value) <= math.MaxInt64 {
			return int64(value), true
		}
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		if value <= math.MaxInt64 {
			return int64(value), true
		}
	}
	return 0, false
}

// AsUint64 accepts all non-negative AMQP integer widths.
func AsUint64(value any) (uint64, bool) {
	switch value := value.(type) {
	case nil:
		return 0, true
	case int:
		if value >= 0 {
			return uint64(value), true
		}
	case int8:
		if value >= 0 {
			return uint64(value), true
		}
	case int16:
		if value >= 0 {
			return uint64(value), true
		}
	case int32:
		if value >= 0 {
			return uint64(value), true
		}
	case int64:
		if value >= 0 {
			return uint64(value), true
		}
	case uint:
		return uint64(value), true
	case uint8:
		return uint64(value), true
	case uint16:
		return uint64(value), true
	case uint32:
		return uint64(value), true
	case uint64:
		return value, true
	}
	return 0, false
}

// AsBool converts the boolean representation used by generated decoders.
func AsBool(value any) (bool, bool) {
	if value == nil {
		return false, true
	}
	result, ok := value.(bool)
	return result, ok
}

// AsList converts an AMQP list and recursively normalizes maps with interface
// keys to maps with string keys.
func AsList(value any) ([]any, bool) {
	if value == nil {
		return nil, true
	}
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]any, len(values))
	for i := range values {
		result[i] = normalizeValue(values[i])
	}
	return result, true
}

// AsStringList converts an AMQP list whose elements are strings.
func AsStringList(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	if values, ok := value.([]string); ok {
		return append([]string(nil), values...), true
	}
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(values))
	for i := range values {
		var itemOK bool
		result[i], itemOK = AsString(values[i])
		if !itemOK {
			return nil, false
		}
	}
	return result, true
}

// AsInt64List converts an AMQP list whose elements are integers.
func AsInt64List(value any) ([]int64, bool) {
	if value == nil {
		return nil, true
	}
	if values, ok := value.([]int64); ok {
		return append([]int64(nil), values...), true
	}
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]int64, len(values))
	for i := range values {
		var itemOK bool
		result[i], itemOK = AsInt64(values[i])
		if !itemOK {
			return nil, false
		}
	}
	return result, true
}

// AsMap converts an AMQP map to its wire-neutral representation.
func AsMap(value any) (map[string]any, bool) {
	return asMap(value, true)
}

func asMap(value any, normalize bool) (map[string]any, bool) {
	if value == nil {
		return nil, true
	}
	switch values := value.(type) {
	case map[string]any:
		if !normalize {
			return values, true
		}
		result := make(map[string]any, len(values))
		for key, item := range values {
			result[key] = normalizeValue(item)
		}
		return result, true
	case map[any]any:
		result := make(map[string]any, len(values))
		for key, item := range values {
			name, ok := key.(string)
			if !ok {
				return nil, false
			}
			if normalize {
				item = normalizeValue(item)
			}
			result[name] = item
		}
		return result, true
	default:
		return nil, false
	}
}

// AsEnum converts either an enum's string value or its numeric ordinal.
func AsEnum(value any, values []string) (string, bool) {
	if value == nil {
		return "", true
	}
	if text, ok := AsString(value); ok {
		for _, allowed := range values {
			if text == allowed {
				return text, true
			}
		}
		return "", false
	}
	ordinal, ok := AsInt64(value)
	if !ok || ordinal < 0 || ordinal >= int64(len(values)) {
		return "", false
	}
	return values[ordinal], true
}

// DecodeError constructs the error returned by generated field decoders.
func DecodeError(typeName, fieldName string, value any) error {
	return fmt.Errorf("cannot decode %s.%s from %T", typeName, fieldName, value)
}

// EqualValues compares list and map values used in generated entities.
func EqualValues(a, b any) bool { return reflect.DeepEqual(a, b) }

func normalizeValue(value any) any {
	if mapped, ok := AsMap(value); ok && mapped != nil {
		return mapped
	}
	if listed, ok := AsList(value); ok && listed != nil {
		return listed
	}
	return value
}
