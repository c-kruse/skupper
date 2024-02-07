package v2_test

import (
	"strings"
	"testing"
	"time"

	v2 "github.com/skupperproject/skupper/pkg/flow/v2"
	"gotest.tools/assert"
)

type tEncodeRecordStub func() (map[uint32]any, error)

func (t tEncodeRecordStub) EncodeRecord() (v2.RecordAttributeSet, error) {
	return t()
}

func (t tEncodeRecordStub) DecodeRecord(v2.RecordAttributeSet) error {
	return nil
}

type tEncodeRecordStubP func() (map[uint32]any, error)

func (t *tEncodeRecordStubP) EncodeRecord() (v2.RecordAttributeSet, error) {
	return (*t)()
}

func (t *tEncodeRecordStubP) DecodeRecord(v2.RecordAttributeSet) error {
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
	v2.MustRegisterRecord(recordTypeEncodeRecordStub, tEncodeRecordStub(nil))
	v2.MustRegisterRecord(recordTypeRecordAttributeEncodeDecode, tRecordAttributeEncodeDecode{})
	v2.MustRegisterRecord(recordTypeEncodeRecordStubP, tEncodeRecordStubP(nil))
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
		v2.MustRegisterRecord(recordTypeEncodeRecordStub, struct{}{})
	})
	t.Run("repeated vflow attribute tags", func(t *testing.T) {
		defer expectPanic(t, `struct field B repeats vflow tag "1" also used by A`)()
		type Repeat struct {
			A int64 `vflow:"1"`
			B int64 `vflow:"1"`
		}
		v2.MustRegisterRecord(recordTypeUnused, Repeat{})
	})
	t.Run("invalid tag", func(t *testing.T) {
		defer expectPanic(t, `vflow struct tag parse error for field A:`)()
		type Repeat struct {
			A int64 `vflow:"identity"`
		}
		v2.MustRegisterRecord(recordTypeUnused, Repeat{})
	})
	t.Run("invalid type", func(t *testing.T) {
		defer expectPanic(t, `invalid vflow field encoder for "D": unsupported attribute type "testing.B"`)()
		type Invalid struct {
			A testing.B
			B *testing.B
			c testing.B `vflow:"3"`
			D testing.B `vflow:"4"`
		}
		v2.MustRegisterRecord(recordTypeUnused, Invalid{})
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
			Input:     v2.SiteRecord{},
			ExpectErr: true,
		}, {
			Name:      "nil",
			ExpectErr: true,
		}, {
			Name:  "basic site",
			Input: v2.SiteRecord{BaseRecord: v2.BaseRecord{Identity: "test"}},
			ExpectedAttrs: map[uint32]any{
				0: uint32(0),
				1: "test",
			},
		}, {
			Name: "full site",
			Input: v2.SiteRecord{
				Location: ptrTo("loc"),
				BaseRecord: v2.BaseRecord{
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
		},
	}
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			actual, err := v2.Encode(tc.Input)
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
