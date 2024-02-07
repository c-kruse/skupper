package encoding

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

var (
	mu        sync.RWMutex
	encodings = map[reflect.Type]*typeEncoding{}
	decodings = map[uint32]*typeEncoding{}
)

const (
	vflowTag     = "vflow"
	typeOfRecord = uint32(0)
)

type typeEncoding struct {
	t         reflect.Type
	codepoint uint32
	encode    encoderFunc
	decode    decoderFunc
}

type encoderFunc func(v reflect.Value) (RecordAttributeSet, error)
type decoderFunc func(attrs RecordAttributeSet, v reflect.Value) error

// MustRegisterRecord registers a record type for Encoding/Decoding panics if
// the type is not compatible, or if the codepoint or type has already been
// registered.
func MustRegisterRecord(codepoint uint32, record interface{}) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := decodings[codepoint]; ok {
		panic(fmt.Sprintf("cannot register record type %T using codepoint %d: already in use", record, codepoint))
	}

	encoding := newEncodingForType(codepoint, reflect.TypeOf(record))
	if existing, ok := encodings[encoding.t]; ok {
		panic(fmt.Sprintf("cannot register same type more than once. type %T already registered with code %d", record, existing.codepoint))
	}
	encodings[encoding.t] = encoding
	decodings[codepoint] = encoding
}

func newEncodingForType(codepoint uint32, t reflect.Type) *typeEncoding {
	encoding := typeEncoding{codepoint: codepoint, t: t}
	{
		// use a pointer to the record type so that we can check if
		// that implements RecordEncoder and/or RecordDecoder
		tPtr := t
		if tPtr.Kind() != reflect.Pointer {
			tPtr = reflect.PointerTo(tPtr)
		}
		// if both encoder and decoder then no need to scan the type
		if tPtr.Implements(recordEncoderType) && tPtr.Implements(recordDeocderType) {
			encoding.encode = encodeRecordEncoder
			encoding.decode = decodeRecordDecoder
			return &encoding
		} else if tPtr.Implements(recordEncoderType) {
			encoding.encode = encodeRecordEncoder
		} else if tPtr.Implements(recordDeocderType) {
			encoding.decode = decodeRecordDecoder
		}
	}

	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("unsupported encoder type %v. Expects struct type or RecordEncoder/RecordDecoder", t))
	}
	se := newStructEncoding(t)
	if encoding.encode == nil {
		encoding.encode = se.encode
	}
	if encoding.decode == nil {
		encoding.decode = se.decode
	}
	return &encoding
}

type field struct {
	Name      string
	Codepoint uint32
	Identity  bool

	index   []int
	encoder fieldEncoder
	decoder fieldDecoder
}

func parseFieldOpts(f reflect.StructField) (field, bool) {
	opts := field{
		Name: f.Name,
	}

	tag := f.Tag.Get(vflowTag)
	if tag == "" {
		return opts, false
	}
	sCode, sOpt, _ := strings.Cut(tag, ",")
	code, err := strconv.ParseUint(sCode, 10, 32)
	if err != nil {
		panic(fmt.Sprintf("vflow struct tag parse error for field %s: %s", f.Name, err))
	}
	opts.Codepoint = uint32(code)
	switch suffix := sOpt; suffix {
	case "identity":
		opts.Identity = true
	case "":
	default:
		panic(fmt.Sprintf("vflow struct tag parse error: unexpected option %s", suffix))
	}
	return opts, true
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
