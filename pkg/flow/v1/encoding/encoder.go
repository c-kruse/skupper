package encoding

import (
	"errors"
	"fmt"
	"reflect"
	"time"
)

var (
	errNotSet = errors.New("field not set")

	recordEncoderType          = reflect.TypeOf((*RecordEncoder)(nil)).Elem()
	recordAttributeEncoderType = reflect.TypeOf((*RecordAttributeEncoder)(nil)).Elem()
	timeType                   = reflect.TypeOf((*time.Time)(nil)).Elem()
)

type RecordAttributeSet = map[uint32]interface{}

// RecordEncoder is implemented by record types that can handle encoding logic
// themselves.
type RecordEncoder interface {
	EncodeRecord() (RecordAttributeSet, error)
}

// RecordAttributeEncoder is implemented by record attribute types that can
// handle encoding themselves.
type RecordAttributeEncoder interface {
	EncodeRecordAttribute() (interface{}, error)
}

// Encode a record into a record attribute set so that it can be sent over the
// vanflow protocol. Only records types that have been registered with
// MustRegisterRecord can be encoded.
func Encode(record any) (RecordAttributeSet, error) {
	mu.RLock()
	defer mu.RUnlock()
	if record == nil {
		return nil, errors.New("cannot encode nil record")
	}
	recordV := reflect.ValueOf(record)
	recordT := recordV.Type()
	encoding, ok := encodings[recordT]
	if !ok {
		if recordT.Kind() == reflect.Pointer {
			recordV = recordV.Elem()
			recordT = recordV.Type()
			encoding, ok = encodings[recordT]
		}
		if !ok {
			return nil, fmt.Errorf("encode error: unregistered record type %T", record)
		}
	}
	result, err := encoding.encode(recordV)
	if result != nil {
		result[typeOfRecord] = encoding.codepoint
	}
	return result, err
}

func encodeRecordEncoder(v reflect.Value) (RecordAttributeSet, error) {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, fmt.Errorf("encoder error: cannot encode nil value")
	}
	if v.Kind() != reflect.Pointer && v.CanAddr() &&
		reflect.PointerTo(v.Type()).Implements(recordEncoderType) {
		v = v.Addr()
	}
	m, ok := v.Interface().(RecordEncoder)
	if !ok {
		panic(fmt.Sprintf("encoder error: type %s does not implement RecordEncoder", v.Type()))
	}
	return m.EncodeRecord()
}

type fieldEncoder interface {
	encode(v reflect.Value) (interface{}, error)
}

func getFieldEncoder(t reflect.Type) (fieldEncoder, error) {
	if t.Implements(recordAttributeEncoderType) {
		return recordAttrFieldEncoder{}, nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		child, err := getFieldEncoder(t.Elem())
		return pointerEncoder{child}, err
	case reflect.String, reflect.Uint64, reflect.Int64, reflect.Uint32, reflect.Int32:
		return rawFieldEncoder{}, nil
	case reflect.Struct:
		if t == timeType {
			return timeEncoder{}, nil
		}
		fallthrough
	default:
		return nil, fmt.Errorf("unsupported attribute type %q", t)
	}
}

type timeEncoder struct{}

func (e timeEncoder) encode(v reflect.Value) (interface{}, error) {
	impl := v.Interface()
	tstamper, ok := impl.(time.Time)
	if !ok {
		panic(fmt.Sprintf("expected time.Time but instead got %T", impl))
	}
	ts := tstamper.UnixMicro()
	if ts < 0 {
		return nil, fmt.Errorf("cannot represent times before epoch in this encoding: %d", ts)
	}
	return uint64(ts), nil
}

type rawFieldEncoder struct {
}

func (e rawFieldEncoder) encode(v reflect.Value) (interface{}, error) {
	if v.IsZero() {
		return nil, errNotSet
	}
	return v.Interface(), nil
}

type pointerEncoder struct {
	sub fieldEncoder
}

func (e pointerEncoder) encode(v reflect.Value) (interface{}, error) {
	if v.IsNil() {
		return nil, errNotSet
	}
	return e.sub.encode(v.Elem())
}

type recordAttrFieldEncoder struct {
}

func (e recordAttrFieldEncoder) encode(v reflect.Value) (interface{}, error) {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, errNotSet
	}
	val := v.Interface()
	encoder, ok := val.(RecordAttributeEncoder)
	if !ok {
		panic(fmt.Sprintf("encoder error: type %s does not implement RecordAttributeEncoder", v.Type()))
	}
	return encoder.EncodeRecordAttribute()
}
