package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"sigs.k8s.io/yaml"
)

type ordered[T any] struct {
	keys   []string
	values map[string]T
}

func (o *ordered[T]) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("expected JSON object, got %v", token)
	}
	o.values = make(map[string]T)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected JSON object key, got %v", token)
		}
		var value T
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		o.keys = append(o.keys, key)
		o.values[key] = value
	}
	_, err = decoder.Token()
	return err
}

type routerSchema struct {
	Prefix      string                `json:"prefix"`
	EntityTypes ordered[entitySchema] `json:"entityTypes"`
}

type entitySchema struct {
	Extends    string                   `json:"extends"`
	Operations []string                 `json:"operations"`
	Attributes ordered[attributeSchema] `json:"attributes"`
}

type attributeSchema struct {
	Type     json.RawMessage `json:"type"`
	Default  json.RawMessage `json:"default"`
	Required bool            `json:"required"`
	Create   bool            `json:"create"`
	Update   bool            `json:"update"`
}

type overrides struct {
	SchemaCommit string            `json:"schemaCommit"`
	Skip         []string          `json:"skip"`
	Core         []string          `json:"core"`
	GoNames      map[string]string `json:"goNames"`
	Types        map[string]string `json:"types"`
	EnumTypes    map[string]string `json:"enumTypes"`
}

type field struct {
	name       string
	goName     string
	constName  string
	goType     string
	kind       string
	required   bool
	create     bool
	update     bool
	defaultRaw json.RawMessage
	fixed      string
	enum       []string
	enumName   string
}

type entity struct {
	key        string
	goName     string
	longName   string
	shortName  string
	core       bool
	operations []string
	fields     []field
}

type generator struct {
	schema    routerSchema
	overrides overrides
	enums     map[string]string
	enumOrder []string
	entities  []entity
	output    string
}

func main() {
	schemaPath := flag.String("schema", "skrouter.json", "path to the router management schema")
	overridesPath := flag.String("overrides", "overrides.yaml", "path to generator overrides")
	output := flag.String("output", ".", "generated file directory")
	flag.Parse()

	var schema routerSchema
	if err := readJSON(*schemaPath, &schema); err != nil {
		fatal(err)
	}
	var custom overrides
	if err := readYAML(*overridesPath, &custom); err != nil {
		fatal(err)
	}
	if custom.SchemaCommit == "" {
		fatal(errors.New("overrides must identify schemaCommit"))
	}

	g := generator{
		schema: schema, overrides: custom, output: *output,
		enums: make(map[string]string),
	}
	if err := g.build(); err != nil {
		fatal(err)
	}
	if err := g.write(); err != nil {
		fatal(err)
	}
}

func readJSON(path string, result any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func readYAML(path string, result any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := json.Unmarshal(jsonData, result); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (g *generator) build() error {
	core := stringSet(g.overrides.Core)
	for _, key := range g.schema.EntityTypes.keys {
		if matchesAny(key, g.overrides.Skip) {
			continue
		}
		fields, err := g.fields(key)
		if err != nil {
			return err
		}
		operations, err := g.operations(key)
		if err != nil {
			return err
		}
		for _, field := range fields {
			if field.update && !contains(operations, "UPDATE") {
				operations = append(operations, "UPDATE")
				break
			}
		}
		if !contains(operations, "QUERY") {
			operations = append([]string{"QUERY"}, operations...)
		}
		for i := range fields {
			fields[i].create = fields[i].create && contains(operations, "CREATE")
			fields[i].update = fields[i].update && contains(operations, "UPDATE")
		}
		name := goName(key)
		if overridden := g.overrides.GoNames[key]; overridden != "" {
			name = overridden
		}
		short := key
		if strings.HasPrefix(short, "router.config.") {
			short = strings.TrimPrefix(short, "router.config.")
		}
		g.entities = append(g.entities, entity{
			key: key, goName: name, longName: g.schema.Prefix + "." + key,
			shortName: short, core: core[key], operations: operations, fields: fields,
		})
	}
	g.assignConstantNames()
	return nil
}

func (g *generator) assignConstantNames() {
	used := make(map[string]bool)
	for _, name := range g.enums {
		used[name] = true
	}
	for _, entity := range g.entities {
		used[entity.goName] = true
		used[entity.goName+"Type"] = true
	}
	for entityIndex := range g.entities {
		entity := &g.entities[entityIndex]
		for fieldIndex := range entity.fields {
			field := &entity.fields[fieldIndex]
			field.constName = entity.goName + field.goName
			if used[field.constName] {
				field.constName += "Field"
			}
			used[field.constName] = true
		}
	}
}

func (g *generator) fields(key string) ([]field, error) {
	lineage, err := g.lineage(key)
	if err != nil {
		return nil, err
	}
	var result []field
	seen := make(map[string]bool)
	for _, owner := range lineage {
		definition := g.schema.EntityTypes.values[owner]
		for _, name := range definition.Attributes.keys {
			if seen[name] {
				return nil, fmt.Errorf("%s inherits duplicate attribute %s", key, name)
			}
			seen[name] = true
			attribute := definition.Attributes.values[name]
			built, err := g.field(key, name, attribute)
			if err != nil {
				return nil, err
			}
			if name == "type" {
				built.fixed = g.schema.Prefix + "." + key
			}
			result = append(result, built)
		}
	}
	return result, nil
}

func (g *generator) field(entityKey, name string, attribute attributeSchema) (field, error) {
	typeName, enum, err := schemaType(attribute.Type)
	if err != nil {
		return field{}, fmt.Errorf("%s.%s: %w", entityKey, name, err)
	}
	overrideKey := entityKey + "." + name
	if overridden := g.overrides.Types[overrideKey]; overridden != "" {
		typeName = overridden
	}
	result := field{
		name: name, goName: goName(name), goType: typeName,
		required: attribute.Required, create: attribute.Create, update: attribute.Update,
		defaultRaw: attribute.Default, enum: enum,
	}
	if len(enum) != 0 {
		signature := strings.Join(enum, "\x00")
		enumName := g.enums[signature]
		requestedName := g.overrides.EnumTypes[overrideKey]
		if enumName == "" {
			enumName = requestedName
			if enumName == "" {
				enumName = goName(entityKey) + goName(name)
			}
			g.enums[signature] = enumName
			g.enumOrder = append(g.enumOrder, signature)
		} else if requestedName != "" && requestedName != enumName {
			return field{}, fmt.Errorf("enum %v named both %s and %s", enum, enumName, requestedName)
		}
		result.goType = enumName
		result.enumName = enumName
	}
	result.kind, err = attributeKind(typeName)
	if err != nil {
		return field{}, fmt.Errorf("%s.%s: %w", entityKey, name, err)
	}
	return result, nil
}

func (g *generator) operations(key string) ([]string, error) {
	lineage, err := g.lineage(key)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, owner := range lineage {
		for _, operation := range g.schema.EntityTypes.values[owner].Operations {
			if !contains(result, operation) {
				result = append(result, operation)
			}
		}
	}
	return result, nil
}

func (g *generator) lineage(key string) ([]string, error) {
	var reversed []string
	seen := make(map[string]bool)
	for key != "" {
		if seen[key] {
			return nil, fmt.Errorf("inheritance cycle at %s", key)
		}
		seen[key] = true
		definition, ok := g.schema.EntityTypes.values[key]
		if !ok {
			return nil, fmt.Errorf("unknown entity type %s", key)
		}
		reversed = append(reversed, key)
		key = definition.Extends
	}
	result := make([]string, len(reversed))
	for i := range reversed {
		result[len(reversed)-1-i] = reversed[i]
	}
	return result, nil
}

func schemaType(raw json.RawMessage) (string, []string, error) {
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		switch scalar {
		case "string", "path", "entityId":
			return "string", nil, nil
		case "integer":
			return "int64", nil, nil
		case "boolean":
			return "bool", nil, nil
		case "list":
			return "[]any", nil, nil
		case "map", "dict", "properties":
			return "map[string]any", nil, nil
		default:
			return "", nil, fmt.Errorf("unsupported schema type %q", scalar)
		}
	}
	var enum []string
	if err := json.Unmarshal(raw, &enum); err != nil || len(enum) == 0 {
		return "", nil, fmt.Errorf("invalid schema type %s", raw)
	}
	return "string", enum, nil
}

func attributeKind(goType string) (string, error) {
	switch goType {
	case "string":
		return "mgmt.StringAttribute", nil
	case "int64":
		return "mgmt.Int64Attribute", nil
	case "uint64":
		return "mgmt.Uint64Attribute", nil
	case "bool":
		return "mgmt.BoolAttribute", nil
	case "[]any", "[]string", "[]int64":
		return "mgmt.ListAttribute", nil
	case "map[string]any":
		return "mgmt.MapAttribute", nil
	default:
		return "", fmt.Errorf("unsupported Go type %q", goType)
	}
}

func (g *generator) write() error {
	entries, err := os.ReadDir(g.output)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "zz_generated.") && strings.HasSuffix(entry.Name(), ".go") {
			if err := os.Remove(filepath.Join(g.output, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := g.writeEnums(); err != nil {
		return err
	}
	for _, entity := range g.entities {
		if err := g.writeEntity(entity); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) writeEnums() error {
	var output bytes.Buffer
	g.header(&output)
	output.WriteString("package entities\n\n")
	for _, signature := range g.enumOrder {
		name := g.enums[signature]
		values := strings.Split(signature, "\x00")
		fmt.Fprintf(&output, "type %s string\n\nconst (\n", name)
		for _, value := range values {
			fmt.Fprintf(&output, "%s%s %s = %s\n", name, goName(value), name, strconv.Quote(value))
		}
		output.WriteString(")\n\n")
		fmt.Fprintf(&output, "var %sValues = []string{", lowerFirst(name))
		for _, value := range values {
			fmt.Fprintf(&output, "%s,", strconv.Quote(value))
		}
		output.WriteString("}\n\n")
	}
	return writeFormatted(filepath.Join(g.output, "zz_generated.enums.go"), output.Bytes())
}

func (g *generator) writeEntity(entity entity) error {
	var output bytes.Buffer
	g.header(&output)
	output.WriteString("package entities\n\n")
	output.WriteString("import \"github.com/skupperproject/skupper/pkg/skrouter/mgmt\"\n\n")

	fmt.Fprintf(&output, "type %s struct {\n", entity.goName)
	for _, field := range entity.fields {
		fmt.Fprintf(&output, "%s %s\n", field.goName, field.goType)
	}
	output.WriteString("}\n\n")

	output.WriteString("const (\n")
	for i, field := range entity.fields {
		output.WriteString(field.constName)
		if i == 0 {
			fmt.Fprintf(&output, " mgmt.Field[%s] = iota", entity.goName)
		}
		output.WriteByte('\n')
	}
	output.WriteString(")\n\n")

	fmt.Fprintf(&output, "var %sType = mgmt.NewType(%s, %s, %t, []string{", entity.goName, strconv.Quote(entity.longName), strconv.Quote(entity.shortName), entity.core)
	for _, operation := range entity.operations {
		fmt.Fprintf(&output, "%s,", strconv.Quote(operation))
	}
	output.WriteString("}, []mgmt.Attribute{\n")
	for _, field := range entity.fields {
		fmt.Fprintf(&output, "{Name:%s, Kind:%s, Required:%t, Create:%t, Update:%t", strconv.Quote(field.name), field.kind, field.required, field.create, field.update)
		if defaultValue, ok, err := defaultLiteral(field); err != nil {
			return fmt.Errorf("%s.%s default: %w", entity.key, field.name, err)
		} else if ok {
			fmt.Fprintf(&output, ", Default:%s, HasDefault:true", defaultValue)
		}
		if len(field.enum) != 0 {
			fmt.Fprintf(&output, ", Enum:%sValues", lowerFirst(field.enumName))
		}
		output.WriteString("},\n")
	}
	output.WriteString("})\n\n")

	writeFieldSet(&output, entity, "AllFields", func(field field) bool { return true })
	writeFieldSet(&output, entity, "CreateFields", func(field field) bool { return field.create })
	writeFieldSet(&output, entity, "UpdateFields", func(field field) bool { return field.update })
	fmt.Fprintf(&output, "func (*%s) ManagementType() *mgmt.Type { return %sType }\n\n", entity.goName, entity.goName)
	writeSetter(&output, entity)
	writeGetter(&output, entity)
	writeEqual(&output, entity)
	writeDiff(&output, entity)
	if err := writeDefaults(&output, entity); err != nil {
		return err
	}
	writeNonZero(&output, entity)

	filename := "zz_generated." + strings.ReplaceAll(strings.ToLower(entity.key), ".", "_") + ".go"
	return writeFormatted(filepath.Join(g.output, filename), output.Bytes())
}

func (g *generator) header(output *bytes.Buffer) {
	output.WriteString("// Code generated by go generate; DO NOT EDIT.\n")
	fmt.Fprintf(output, "// Skupper Router schema commit: %s\n\n", g.overrides.SchemaCommit)
}

func writeFieldSet(output *bytes.Buffer, entity entity, suffix string, include func(field) bool) {
	var included []field
	for _, field := range entity.fields {
		if include(field) {
			included = append(included, field)
		}
	}
	if len(included) == 0 {
		fmt.Fprintf(output, "var %s%s mgmt.FieldSet[%s]\n\n", entity.goName, suffix, entity.goName)
		return
	}
	fmt.Fprintf(output, "var %s%s = mgmt.Fields(", entity.goName, suffix)
	for _, field := range included {
		fmt.Fprintf(output, "%s,", field.constName)
	}
	output.WriteString(")\n\n")
}

func writeSetter(output *bytes.Buffer, entity entity) {
	fmt.Fprintf(output, "func (e *%s) SetManagementField(field mgmt.Field[%s], value any) error {\nswitch field {\n", entity.goName, entity.goName)
	for _, field := range entity.fields {
		fmt.Fprintf(output, "case %s:\n", field.constName)
		switch {
		case len(field.enum) != 0:
			fmt.Fprintf(output, "decoded, ok := mgmt.AsEnum(value, %sValues)\nif !ok { return mgmt.DecodeError(%sType.Name, %s, value) }\ne.%s = %s(decoded)\n", lowerFirst(field.enumName), entity.goName, strconv.Quote(field.name), field.goName, field.goType)
		case field.goType == "string":
			writeDecodeAssignment(output, entity, field, "AsString")
		case field.goType == "int64":
			writeDecodeAssignment(output, entity, field, "AsInt64")
		case field.goType == "uint64":
			writeDecodeAssignment(output, entity, field, "AsUint64")
		case field.goType == "bool":
			writeDecodeAssignment(output, entity, field, "AsBool")
		case field.goType == "[]any":
			writeDecodeAssignment(output, entity, field, "AsList")
		case field.goType == "[]string":
			writeDecodeAssignment(output, entity, field, "AsStringList")
		case field.goType == "[]int64":
			writeDecodeAssignment(output, entity, field, "AsInt64List")
		case field.goType == "map[string]any":
			writeDecodeAssignment(output, entity, field, "AsMap")
		default:
			panic("unsupported setter type " + field.goType)
		}
		output.WriteString("return nil\n")
	}
	fmt.Fprintf(output, "}\nreturn mgmt.DecodeError(%sType.Name, \"<unknown>\", value)\n}\n\n", entity.goName)
}

func writeDecodeAssignment(output *bytes.Buffer, entity entity, field field, conversion string) {
	fmt.Fprintf(output, "decoded, ok := mgmt.%s(value)\nif !ok { return mgmt.DecodeError(%sType.Name, %s, value) }\ne.%s = decoded\n", conversion, entity.goName, strconv.Quote(field.name), field.goName)
}

func writeGetter(output *bytes.Buffer, entity entity) {
	fmt.Fprintf(output, "func (e *%s) ManagementValue(field mgmt.Field[%s]) any {\nswitch field {\n", entity.goName, entity.goName)
	for _, field := range entity.fields {
		fmt.Fprintf(output, "case %s:\n", field.constName)
		if len(field.enum) != 0 {
			fmt.Fprintf(output, "return string(e.%s)\n", field.goName)
		} else {
			fmt.Fprintf(output, "return e.%s\n", field.goName)
		}
	}
	output.WriteString("}\nreturn nil\n}\n\n")
}

func writeEqual(output *bytes.Buffer, entity entity) {
	fmt.Fprintf(output, "func (e *%s) Equal(other *%s, fields mgmt.FieldSet[%s]) bool {\n", entity.goName, entity.goName, entity.goName)
	for _, field := range entity.fields {
		fmt.Fprintf(output, "if fields.Has(%s) && ", field.constName)
		if isComparable(field.goType) {
			fmt.Fprintf(output, "e.%s != other.%s", field.goName, field.goName)
		} else {
			fmt.Fprintf(output, "!mgmt.EqualValues(e.%s, other.%s)", field.goName, field.goName)
		}
		output.WriteString(" { return false }\n")
	}
	output.WriteString("return true\n}\n\n")
}

func writeDiff(output *bytes.Buffer, entity entity) {
	fmt.Fprintf(output, "func (e *%s) Diff(other *%s, fields mgmt.FieldSet[%s]) mgmt.FieldSet[%s] {\nvar result mgmt.FieldSet[%s]\n", entity.goName, entity.goName, entity.goName, entity.goName, entity.goName)
	for _, field := range entity.fields {
		fmt.Fprintf(output, "if fields.Has(%s) && ", field.constName)
		if isComparable(field.goType) {
			fmt.Fprintf(output, "e.%s != other.%s", field.goName, field.goName)
		} else {
			fmt.Fprintf(output, "!mgmt.EqualValues(e.%s, other.%s)", field.goName, field.goName)
		}
		fmt.Fprintf(output, " { result = result.Union(mgmt.Fields(%s)) }\n", field.constName)
	}
	output.WriteString("return result\n}\n\n")
}

func writeDefaults(output *bytes.Buffer, entity entity) error {
	fmt.Fprintf(output, "func (e *%s) ApplyDefaults(present mgmt.FieldSet[%s]) {\n", entity.goName, entity.goName)
	for _, field := range entity.fields {
		value, ok, err := defaultLiteral(field)
		if err != nil {
			return fmt.Errorf("%s.%s default: %w", entity.key, field.name, err)
		}
		if ok {
			fmt.Fprintf(output, "if !present.Has(%s) { e.%s = %s }\n", field.constName, field.goName, value)
		}
	}
	output.WriteString("}\n\n")
	return nil
}

func writeNonZero(output *bytes.Buffer, entity entity) {
	fmt.Fprintf(output, "func (e *%s) NonZeroFields() mgmt.FieldSet[%s] {\nvar result mgmt.FieldSet[%s]\n", entity.goName, entity.goName, entity.goName)
	for _, field := range entity.fields {
		condition := "e." + field.goName + " != 0"
		switch {
		case field.goType == "string" || len(field.enum) != 0:
			condition = "e." + field.goName + " != \"\""
		case field.goType == "bool":
			condition = "e." + field.goName
		case strings.HasPrefix(field.goType, "[]") || strings.HasPrefix(field.goType, "map["):
			condition = "len(e." + field.goName + ") != 0"
		}
		fmt.Fprintf(output, "if %s { result = result.Union(mgmt.Fields(%s)) }\n", condition, field.constName)
	}
	output.WriteString("return result\n}\n")
}

func defaultLiteral(field field) (string, bool, error) {
	if field.fixed != "" {
		return strconv.Quote(field.fixed), true, nil
	}
	if len(field.defaultRaw) == 0 || bytes.Equal(field.defaultRaw, []byte("null")) {
		return "", false, nil
	}
	switch {
	case len(field.enum) != 0:
		var value string
		if err := json.Unmarshal(field.defaultRaw, &value); err != nil {
			return "", false, err
		}
		return field.goType + "(" + strconv.Quote(value) + ")", true, nil
	case field.goType == "string":
		var value any
		decoder := json.NewDecoder(bytes.NewReader(field.defaultRaw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return "", false, err
		}
		switch value := value.(type) {
		case string:
			return strconv.Quote(value), true, nil
		case json.Number:
			return strconv.Quote(value.String()), true, nil
		default:
			return "", false, fmt.Errorf("cannot convert %T to string", value)
		}
	case field.goType == "bool":
		var value bool
		if err := json.Unmarshal(field.defaultRaw, &value); err != nil {
			return "", false, err
		}
		return strconv.FormatBool(value), true, nil
	case field.goType == "int64" || field.goType == "uint64":
		var value json.Number
		decoder := json.NewDecoder(bytes.NewReader(field.defaultRaw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return "", false, err
		}
		return field.goType + "(" + value.String() + ")", true, nil
	default:
		return "", false, fmt.Errorf("unsupported default for %s", field.goType)
	}
}

func isComparable(goType string) bool {
	return !strings.HasPrefix(goType, "[]") && !strings.HasPrefix(goType, "map[")
}

func writeFormatted(path string, source []byte) error {
	formatted, err := format.Source(source)
	if err != nil {
		return fmt.Errorf("format %s: %w\n%s", path, err, source)
	}
	return os.WriteFile(path, formatted, 0o644)
}

func goName(value string) string {
	var words []string
	var current []rune
	runes := []rune(value)
	flush := func() {
		if len(current) != 0 {
			words = append(words, string(current))
			current = nil
		}
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(current) != 0 && unicode.IsUpper(r) {
			previous := runes[i-1]
			var next rune
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && unicode.IsLower(next)) {
				flush()
			}
		}
		current = append(current, r)
	}
	flush()
	var result strings.Builder
	for _, word := range words {
		lower := strings.ToLower(word)
		if lower == "" {
			continue
		}
		runes := []rune(lower)
		result.WriteRune(unicode.ToUpper(runes[0]))
		result.WriteString(string(runes[1:]))
	}
	name := result.String()
	if name == "" {
		return "Value"
	}
	if unicode.IsDigit([]rune(name)[0]) {
		return "Value" + name
	}
	return name
}

func lowerFirst(value string) string {
	runes := []rune(value)
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func matchesAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(value, strings.TrimSuffix(pattern, "*")) {
				return true
			}
		} else if value == pattern {
			return true
		}
	}
	return false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
