package encoding_test

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/skupperproject/skupper/pkg/flow/v1"
	encoding "github.com/skupperproject/skupper/pkg/flow/v1/encoding"
	"gotest.tools/assert"
)

type tEncodeRecordStub func() (map[uint32]any, error)

func (t tEncodeRecordStub) EncodeRecord() (encoding.RecordAttributeSet, error) {
	return t()
}

func (t tEncodeRecordStub) DecodeRecord(encoding.RecordAttributeSet) error {
	return nil
}

type tEncodeRecordStubP func() (map[uint32]any, error)

func (t *tEncodeRecordStubP) EncodeRecord() (encoding.RecordAttributeSet, error) {
	return (*t)()
}

func (t *tEncodeRecordStubP) DecodeRecord(encoding.RecordAttributeSet) error {
	return nil
}

type tRecordAttributeEncodeDecode struct {
	A *MagicBool `vflow:"99"`
	B MagicBool  `vflow:"100"`
}

type MagicBool bool

func (m MagicBool) EncodeRecordAttribute() (interface{}, error) {
	if m {
		return "OKAY", nil
	}
	return "ERROR", nil
}

func (m *MagicBool) DecodeRecordAttribute(obj interface{}) error {
	if obj.(string) == "OKAY" {
		(*m) = true
	}
	return nil
}

const (
	recordTypeUnused                      uint32 = 99900
	recordTypeEncodeRecordStub            uint32 = 99901
	recordTypeEncodeRecordStubP           uint32 = 99902
	recordTypeRecordAttributeEncodeDecode uint32 = 99903
)

func init() {
	encoding.MustRegisterRecord(recordTypeEncodeRecordStub, tEncodeRecordStub(nil))
	encoding.MustRegisterRecord(recordTypeRecordAttributeEncodeDecode, tRecordAttributeEncodeDecode{})
	encoding.MustRegisterRecord(recordTypeEncodeRecordStubP, tEncodeRecordStubP(nil))
}

func TestMustRegister(t *testing.T) {
	expectPanic := func(t *testing.T, expected string) func() {
		return func() {
			if recovered := recover(); recovered != nil {
				assert.Check(t, strings.HasPrefix(recovered.(string), expected), "got %q but expected %q", recovered, expected)
				return
			}
			t.Fatal("expected MustRegister to panic")
		}
	}
	t.Run("existing codepoint", func(t *testing.T) {
		defer expectPanic(t, "cannot register record type struct {} using codepoint 99901: already in use")()
		encoding.MustRegisterRecord(recordTypeEncodeRecordStub, struct{}{})
	})
	t.Run("repeated vflow attribute tags", func(t *testing.T) {
		defer expectPanic(t, `struct field B repeats vflow tag "1" also used by A`)()
		type Repeat struct {
			A int64 `vflow:"1"`
			B int64 `vflow:"1"`
		}
		encoding.MustRegisterRecord(recordTypeUnused, Repeat{})
	})
	t.Run("invalid tag", func(t *testing.T) {
		defer expectPanic(t, `vflow struct tag parse error for field A:`)()
		type Repeat struct {
			A int64 `vflow:"identity"`
		}
		encoding.MustRegisterRecord(recordTypeUnused, Repeat{})
	})
	t.Run("invalid type", func(t *testing.T) {
		defer expectPanic(t, `invalid vflow field encoder for "D": unsupported attribute type "testing.B"`)()
		type Invalid struct {
			A testing.B
			B *testing.B
			c testing.B `vflow:"3"`
			D testing.B `vflow:"4"`
		}
		encoding.MustRegisterRecord(recordTypeUnused, Invalid{c: testing.B{}})
	})
	t.Run("register twice", func(t *testing.T) {
		defer expectPanic(t, `cannot register same type more than once. type encoding_test.tEncodeRecordStub already registered with code`)()
		encoding.MustRegisterRecord(recordTypeUnused, tEncodeRecordStub(nil))
	})
}
func TestMarshal(t *testing.T) {
	timeA := time.UnixMicro(100)
	timeB := time.UnixMicro(333)
	testCases := []struct {
		Name          string
		Input         interface{}
		ExpectErr     bool
		ExpectedAttrs map[uint32]interface{}
	}{
		{
			Name:      "missing identity error",
			Input:     v1.SiteRecord{},
			ExpectErr: true,
		}, {
			Name:      "nil",
			ExpectErr: true,
		}, {
			Name:  "basic site",
			Input: v1.SiteRecord{BaseRecord: v1.BaseRecord{Identity: "test"}},
			ExpectedAttrs: map[uint32]any{
				0: uint32(0),
				1: "test",
			},
		}, {
			Name: "full site",
			Input: v1.SiteRecord{
				Location: ptrTo("loc"),
				BaseRecord: v1.BaseRecord{
					Identity:  "test",
					StartTime: ptrTo(timeA),
					EndTime:   ptrTo(timeB),
				}},
			ExpectedAttrs: map[uint32]any{
				0: uint32(0),
				1: "test",
				3: uint64(100),
				4: uint64(333),
				9: "loc",
			},
		}, {
			Name: "RecordMarshaler",
			Input: tEncodeRecordStub(func() (map[uint32]any, error) {
				return map[uint32]any{1: "test", 2: 222, 3: ""}, nil
			}),
			ExpectedAttrs: map[uint32]any{
				0: recordTypeEncodeRecordStub, // type
				1: "test",
				2: 222,
				3: "",
			},
		}, {
			Name: "RecordMarshaler",
			Input: ptrTo(tEncodeRecordStub(func() (map[uint32]any, error) {
				return map[uint32]any{1: "test", 2: 222, 3: ""}, nil
			})),
			ExpectedAttrs: map[uint32]any{
				0: recordTypeEncodeRecordStub, // type
				1: "test",
				2: 222,
				3: "",
			},
		}, {
			Name: "RecordMarshaler pointer",
			Input: ptrTo(tEncodeRecordStub(func() (map[uint32]any, error) {
				return map[uint32]any{1: "test", 2: 222}, nil
			})),
			ExpectedAttrs: map[uint32]any{
				0: recordTypeEncodeRecordStub, // type
				1: "test",
				2: 222,
			},
		}, {
			Name: "RecordAttributeMarshaler",
			Input: ptrTo(tRecordAttributeEncodeDecode{
				A: ptrTo(MagicBool(true)),
				B: MagicBool(false),
			}),
			ExpectedAttrs: map[uint32]any{
				0:   recordTypeRecordAttributeEncodeDecode, // type
				99:  "OKAY",
				100: "ERROR",
			},
		}, {
			Name: "PtrRecordMarshaler",
			Input: ptrTo(tEncodeRecordStubP(func() (map[uint32]any, error) {
				return map[uint32]any{1: "test", 2: 222}, nil
			})),
			ExpectedAttrs: map[uint32]any{
				0: recordTypeEncodeRecordStubP, // type
				1: "test",
				2: 222,
			},
		}, {
			Name:      "unregistered type",
			Input:     testing.B{},
			ExpectErr: true,
		}, {
			Name:      "time before epoch",
			Input:     v1.SiteRecord{BaseRecord: v1.BaseRecord{Identity: "test", StartTime: ptrTo(time.UnixMicro(-1))}},
			ExpectErr: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			actual, err := encoding.Encode(tc.Input)
			if tc.ExpectErr {
				assert.Check(t, err != nil)
			} else {
				assert.Check(t, err)
				assert.DeepEqual(t, tc.ExpectedAttrs, map[uint32]any(actual))
			}
		})
	}
}

func ptrTo[T any](t T) *T {
	return &t
}
