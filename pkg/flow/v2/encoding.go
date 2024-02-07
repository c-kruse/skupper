package v2

import (
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

func MustRegisterRecord(codepoint uint32, record interface{}) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := decodings[codepoint]; ok {
		panic(fmt.Sprintf("cannot register record type %T using codepoint %d: already in use", record, codepoint))
	}

	encoding := newEncodingForType(codepoint, reflect.TypeOf(record))
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
