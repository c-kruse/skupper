package v2_test

import (
	"fmt"
	"math"
	"path"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v2 "github.com/skupperproject/skupper/pkg/flow/v2"
)

type tDecodeRecord struct{}

func (r tDecodeRecord) DecodeRecord(attrs v2.RecordAttributeSet) error {
	return fmt.Errorf("input: %s", attrs[1])
}

type tDecodeRecordP struct {
	A string `vflow:"1"`
	B string `vflow:"2"`
}

func (r *tDecodeRecordP) DecodeRecord(attrs v2.RecordAttributeSet) error {
	if a, ok := attrs[1]; ok {
		r.A = a.(string)
	}
	if b, ok := attrs[2]; ok {
		r.B = strings.Repeat("*", len(b.(string)))
	}
	return nil
}

type tNestedEmbedded struct {
	A
	Root uint32 `vflow:"1"`
}
type A struct {
	B
	X
}
type B struct {
	B string `vflow:"100,identity"`
}
type X struct {
	X string `vflow:"10,identity"`
}

const (
	recordTypeDecodeRecord   uint32 = 88801
	recordTypeDecodeRecordP  uint32 = 88802
	recordTypeNestedEmbedded uint32 = 88803
)

func init() {
	v2.MustRegisterRecord(recordTypeDecodeRecord, tDecodeRecord{})
	v2.MustRegisterRecord(recordTypeDecodeRecordP, tDecodeRecordP{})
	v2.MustRegisterRecord(recordTypeNestedEmbedded, tNestedEmbedded{})
}

var decoderTests = []struct {
	CaseName
	In       map[uint32]interface{}
	ErrorMsg string
	Out      interface{}
	Golden   bool
}{
	{
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: uint32(1), 1: "routerid", 2: "parentid", 3: uint64(100), 4: uint64(1000),
			12: "default", 30: "routername",
		},
		Out: &v2.RouterRecord{
			BaseRecord: v2.BaseRecord{
				Identity: "routerid", StartTime: ptrTo(time.UnixMicro(100)), EndTime: ptrTo(time.UnixMicro(1000)),
			},
			Parent:    ptrTo("parentid"),
			Namespace: ptrTo("default"),
			Name:      ptrTo("routername"),
		},
		Golden: true,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: uint32(404_404), 12: "unknown",
		},
		ErrorMsg: "decode error: unknown record type for 404404",
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			12: "unknown",
		},
		ErrorMsg: "decode error: record type attribute not present",
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: "unknown",
		},
		ErrorMsg: `decode error: unexpected type for record type attribute "string"`,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: uint32(1),
		},
		ErrorMsg: `decode error: record attribute set missing identity field "Identity"`,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: uint32(1),
		},
		ErrorMsg: `decode error: record attribute set missing identity field "Identity"`,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: recordTypeRecordAttributeEncodeDecode, 99: "OKAY", 100: "OKAY",
		},
		Out:    &tRecordAttributeEncodeDecode{A: ptrTo(MagicBool(true)), B: MagicBool(true)},
		Golden: true,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: recordTypeRecordAttributeEncodeDecode,
		},
		Out: &tRecordAttributeEncodeDecode{},
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: uint32(1), 1: "routerid",
			4: uint64(math.MaxInt64) + 1,
		},
		ErrorMsg: `decode error: error decoding field "EndTime": time too far in future for internal representation: 0x8000000000000000`,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: uint32(recordTypeDecodeRecord), 1: "testinput",
		},
		ErrorMsg: `decode error: input: testinput`,
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: recordTypeDecodeRecordP, 1: "test", 2: "password",
		},
		Out: &tDecodeRecordP{A: "test", B: "********"},
	}, {
		CaseName: Name(""),
		In: map[uint32]interface{}{
			0: recordTypeNestedEmbedded, 1: uint32(1), 10: "ten", 100: "100",
		},
		Out: &tNestedEmbedded{Root: 1, A: A{B: B{B: "100"}, X: X{X: "ten"}}},
	},
}

func TestDecode(t *testing.T) {
	for _, tc := range decoderTests {
		t.Run(tc.Name, func(t *testing.T) {
			out, err := v2.Decode(tc.In)
			if !equalError(err, tc.ErrorMsg) {
				t.Fatalf("%s: unexpected error. wanted: %q but got: %q", tc.Where, tc.ErrorMsg, err)
			}
			if tc.Out == nil {
				return
			}

			if !cmp.Equal(tc.Out, out) {
				t.Fatalf("%s: Decode got: %+v want: %+v\n\n%s", tc.Where, out, tc.Out, cmp.Diff(tc.Out, out))
			}

			if tc.ErrorMsg != "" || !tc.Golden {
				return
			}

			remarshaled, err := v2.Encode(out)
			if err != nil {
				t.Fatalf("%s: unexpected encode error: %q", tc.Where, err)
			}
			if !cmp.Equal(tc.In, remarshaled) {
				t.Fatalf("%s: Encode got: %+v want: %+v\n\n%s", tc.Where, remarshaled, tc.In, cmp.Diff(tc.In, remarshaled))
			}

		})
	}
}

func equalError(e error, s string) bool {
	if e == nil || s == "" {
		return e == nil && s == ""
	}
	return e.Error() == s
}

type CaseName struct {
	Name  string
	Where CasePos
}

func Name(name string) CaseName {
	c := CaseName{Name: name}
	runtime.Callers(2, c.Where[:])
	return c
}

type CasePos [1]uintptr

func (p CasePos) String() string {
	frames := runtime.CallersFrames(p[:])
	next, _ := frames.Next()
	return fmt.Sprintf("%s:%d", path.Base(next.File), next.Line)
}
