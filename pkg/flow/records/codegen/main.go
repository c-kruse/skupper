package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

var (
	Package string

	toTitleCaser = cases.Title(language.Und, cases.NoLower)
)

func init() {
	flag.Usage = func() {
		fmt.Printf(`Usage of %s:

%s [flags] [spec file]
`, os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&Package, "package", "records", "name of go package to generate")
}

func main() {
	flag.Parse()
	if len(flag.Args()) != 1 {
		flag.Usage()
		os.Exit(1)
	}
	fileName := flag.Arg(0)
	info, err := os.Stat(fileName)
	if err != nil || info.IsDir() {
		fmt.Printf("could not open file %s\n", fileName)
		flag.Usage()
		os.Exit(1)
	}
	f, err := os.Open(fileName)
	if err != nil {
		fmt.Printf("failed to open file %s: %s", fileName, err)
		os.Exit(1)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	var spec Root
	dec.Decode(&spec)

	recordsW, err := os.OpenFile("records.go", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Printf("failed to open file %s: %s", "records.go", err)
		os.Exit(1)
	}
	recordsTstW, err := os.OpenFile("records_test.go", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Printf("failed to open file %s: %s", "records_test.go", err)
		os.Exit(1)
	}
	records := Parse(spec)
	GenerateRecordsCode(records, recordsW)
	GenerateRecordsTestCode(records, recordsTstW)

}

func Parse(root Root) []RecordTypeInfo {
	recordDefs := make([]RecordTypeInfo, 0, len(root.Spec))
	for name, spec := range root.Spec {
		code := spec.(map[any]any)["code"].(int)
		var attributes []RecordAttributeInfo
		for _, attrSpec := range spec.(map[any]any)["attributes"].([]any) {
			attrSpecMap := attrSpec.(map[any]any)
			info := RecordAttributeInfo{
				Code: uint(attrSpecMap["code"].(int)),
			}
			if attrName, ok := attrSpecMap["identity"]; ok {
				info.Type = "string"
				info.Name = attrName.(string)
			} else if attrName, ok := attrSpecMap["string"]; ok {
				info.Type = "string"
				info.Name = attrName.(string)
			} else if attrName, ok := attrSpecMap["uint"]; ok {
				info.Type = "uint64"
				info.Name = attrName.(string)
			}
			attributes = append(attributes, info)
		}
		sort.Slice(attributes, func(i, j int) bool { return attributes[i].Code < attributes[j].Code })
		recordDefs = append(recordDefs, RecordTypeInfo{
			Name:       name,
			Code:       uint(code),
			Attributes: attributes,
		})
	}
	sort.Slice(recordDefs, func(i, j int) bool { return recordDefs[i].Code < recordDefs[j].Code })
	return recordDefs
}

type Root struct {
	Spec map[string]interface{} `yaml:"spec"`
}

type RecordAttributeInfo struct {
	Name string
	Code uint
	Type string
}
type RecordTypeInfo struct {
	Name       string
	Code       uint
	Attributes []RecordAttributeInfo
}

func GenerateRecordsCode(recordDefs []RecordTypeInfo, w io.Writer) {
	P := func(fargs ...any) {
		fmt.Fprintln(w, fargs...)
	}
	P("// Code is generated. DO NOT EDIT")
	P()
	P("package ", Package)
	P()
	P(`import "fmt"`)
	P()
	P(`type recordType uint32`)
	P()
	P(`// RecordType codes`)
	P(`const (`)
	for _, r := range recordDefs {
		P("	", r.Name, "	= ", r.Code)
	}
	P(`)`)
	P()
	P(`var recordDecoders = map[recordType]recordDecoder{`)
	for _, r := range recordDefs {
		decodeFnName := "decode" + toTitleCaser.String(r.Name)
		P("	", r.Name+":", decodeFnName+",")
	}
	P(`}`)
	P()
	for _, r := range recordDefs {
		recordName := r.Name
		RecordName := toTitleCaser.String(r.Name) + "Record"
		fnDecodeName := "decode" + toTitleCaser.String(r.Name)

		P("type", RecordName, "struct {")
		P("	BaseRecord")
		for _, attr := range r.Attributes {
			AttrName := toTitleCaser.String(attr.Name)
			P("	", AttrName, "*"+attr.Type, "//", attr.Code)
		}
		P("}")
		P()
		P(`func (r`, RecordName+`)`, `Encode() map[uint32]any {`)
		P(`	attributeSet := r.BaseRecord.Encode()`)
		P(`	attributeSet[typeOfRecord] =`, "uint64("+r.Name+")")
		for _, attr := range r.Attributes {
			AttrName := toTitleCaser.String(attr.Name)
			P(`	setOpt(attributeSet,`, fmt.Sprint(attr.Code)+`,`, `r.`+AttrName+`)`)
		}
		P(`	return attributeSet`)
		P("}")
		P()
		P(`func`, fnDecodeName+`(attributeSet map[any]any) (Record, error) {`)
		P(`	var err error`)
		P(`	var`, recordName, RecordName)
		P(`	if`, recordName+`.BaseRecord, err = decodeBase(attributeSet); err != nil {`)
		P(`		return`, recordName+`, err`)
		P(`	}`)
		for _, attr := range r.Attributes {
			AttrName := toTitleCaser.String(attr.Name)
			P(`	if`, recordName+`.`+AttrName+`,`, `err = getFromAttributeSetOpt[`+attr.Type+`](attributeSet,`, fmt.Sprint(attr.Code)+`); err != nil {`)
			P(`		return`, recordName+`,`, `fmt.Errorf("error getting record`, AttrName, `attribute: %w", err)`)
			P(`	}`)
		}
		P(`	return`, recordName+",", `nil`)
		P(`}`)
		P()
	}
}

func GenerateRecordsTestCode(recordDefs []RecordTypeInfo, w io.Writer) {
	P := func(fargs ...any) {
		fmt.Fprintln(w, fargs...)
	}
	P("// Code is generated. DO NOT EDIT")
	P()
	P("package ", Package)
	P(`
import (
	"testing"

	"gotest.tools/assert"
)

// TestRecordDecoders checks that a record that has been decoded then reencoded
// is identical to the original
func TestRecordDecoders(t *testing.T) {
	testCases := []struct {
		Name       string
		RecordCode recordType
		Map        map[any]any
	}{`)
	for _, r := range recordDefs {
		recordName := r.Name
		RecordName := toTitleCaser.String(r.Name) + "Record"
		P(`	{`)
		P(`		Name:	"` + RecordName + `",`)
		P(`		RecordCode:	` + recordName + `,`)
		P(`		Map:	map[any]any{`)
		P(`			typeOfRecord:	uint64(` + recordName + `), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),`)
		for _, attr := range r.Attributes {
			attrValue := fmt.Sprintf(`"%s-val"`, attr.Name)
			if attr.Type == "uint64" {
				attrValue = fmt.Sprintf(`uint64(%d)`, 100+attr.Code)
			}
			P(`uint32(`+fmt.Sprint(attr.Code)+`):`, attrValue+`,`)
		}
		P(`		},`)
		P(`	},`)
	}
	P(`}`)
	P(`
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			r, err := recordDecoders[tc.RecordCode](tc.Map)
			assert.Assert(t, err)
			attrSet := make(map[any]any)
			for k, v := range r.Encode() {
				attrSet[k] = v
			}
			assert.DeepEqual(t, tc.Map, attrSet)
		})
	}
}`)
}
