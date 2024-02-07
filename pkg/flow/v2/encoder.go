package v2

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

type RecordEncoder interface {
	EncodeRecord() (RecordAttributeSet, error)
}
type RecordAttributeEncoder interface {
	EncodeRecordAttribute() (interface{}, error)
}

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

func fieldsForType(t reflect.Type, index []int) []field {
	var fields []field
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		next := make([]int, len(index)+1)
		copy(next, index)
		next[len(next)-1] = i
		// Include fields from embedded structs
		if f.Anonymous {
			if f.Type.Kind() == reflect.Struct {
				fields = append(fields, fieldsForType(f.Type, next)...)
			}
			continue
		} else if !f.IsExported() {
			// ignore unexported fields
			continue
		}

		fieldOpts, ok := parseFieldOpts(f)
		if !ok {
			continue
		}
		fieldOpts.index = next
		encoder, err := getFieldEncoder(f.Type)
		if err != nil {
			panic(fmt.Sprintf("invalid vflow field encoder for %q: %s", f.Name, err))
		}
		fieldOpts.encoder = encoder
		decoder, err := getFieldDecoder(f.Type)
		if err != nil {
			panic(fmt.Sprintf("invalid vflow field decoder for %q: %s", f.Name, err))
		}
		fieldOpts.decoder = decoder
		fields = append(fields, fieldOpts)
	}
	return fields
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

func newStructEncoding(t reflect.Type) structEncoding {
	var encoder structEncoding
	fields := fieldsForType(t, nil)
	codepoints := map[uint32]field{}
	for _, field := range fields {
		if existing, ok := codepoints[field.Codepoint]; ok {
			panic(fmt.Sprintf(
				"struct field %s repeats vflow tag \"%d\" also used by %s",
				field.Name, field.Codepoint, existing.Name))
		}
		codepoints[field.Codepoint] = field
	}
	encoder.fields = fields
	return encoder
}

type structEncoding struct {
	fields []field
}

func (s structEncoding) encode(root reflect.Value) (RecordAttributeSet, error) {
	attributeSet := make(RecordAttributeSet)
	var v reflect.Value
	for _, field := range s.fields {
		v = root
		for _, idx := range field.index {
			v = v.Field(idx)
		}
		out, err := field.encoder.encode(v)
		if err != nil {
			if errors.Is(err, errNotSet) {
				if field.Identity {
					return attributeSet, fmt.Errorf("missing or empty field %s part of record identity: %s", field.Name, err)
				}
				continue // ignore if not part of identity
			}
			return attributeSet, fmt.Errorf("error encoding field %q: %w", field.Name, err)
		}
		if out != nil {
			attributeSet[field.Codepoint] = out
		}
	}
	return attributeSet, nil
}

func (s structEncoding) decode(attrs RecordAttributeSet, root reflect.Value) error {
	var v reflect.Value
	for _, field := range s.fields {
		obj, ok := attrs[field.Codepoint]
		if !ok {
			if field.Identity {
				return fmt.Errorf("record attribute set missing identity field %q", field.Name)
			}
			continue
		}
		v = root.Elem()
		for _, idx := range field.index {
			v = v.Field(idx)
		}
		err := field.decoder.decode(obj, v)
		if err != nil {
			return fmt.Errorf("error decoding field %q: %w", field.Name, err)
		}
	}
	return nil
}
