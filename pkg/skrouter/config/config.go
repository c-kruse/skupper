// Package config reads and writes skrouterd's tuple-array JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/skupperproject/skupper/pkg/skrouter/mgmt"
	"github.com/skupperproject/skupper/pkg/skrouter/mgmt/entities"
)

// Document is a router configuration. Fields records exactly the attributes
// supplied by the configuration, including attributes whose value is zero.
// A Router with no fields is omitted, allowing bridge-only documents.
// Required attributes and cross-entity constraints are validated by the router.
type Document struct {
	Router            mgmt.Partial[entities.Router]
	Site              *mgmt.Partial[entities.Site]
	SslProfiles       []mgmt.Partial[entities.SslProfile]
	ProxyProfiles     []mgmt.Partial[entities.ProxyProfile]
	Listeners         []mgmt.Partial[entities.Listener]
	Connectors        []mgmt.Partial[entities.Connector]
	Addresses         []mgmt.Partial[entities.ConfigAddress]
	TcpListeners      []mgmt.Partial[entities.TcpListener]
	TcpConnectors     []mgmt.Partial[entities.TcpConnector]
	ListenerAddresses []mgmt.Partial[entities.ListenerAddress]
	Logs              []mgmt.Partial[entities.Log]
}

type wireTuple struct {
	section string
	value   orderedObject
}

// Parse parses skrouterd tuple-array JSON. It rejects unknown sections and
// attributes, accepts numeric strings and boolean spellings such as yes/no,
// and does not apply schema defaults.
func Parse(data []byte) (Document, error) {
	var result Document
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '[' {
		return result, fmt.Errorf("router config: top level must be an array")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var tuples []json.RawMessage
	if err := decoder.Decode(&tuples); err != nil {
		return result, fmt.Errorf("router config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return result, fmt.Errorf("router config: trailing JSON value")
		}
		return result, fmt.Errorf("router config: trailing data: %w", err)
	}
	routerSeen := false
	for i, raw := range tuples {
		var tuple []json.RawMessage
		if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
			return Document{}, fmt.Errorf("router config tuple %d: expected [section, attributes]", i)
		}
		var section string
		if err := json.Unmarshal(tuple[0], &section); err != nil {
			return Document{}, fmt.Errorf("router config tuple %d: section must be a string", i)
		}
		switch section {
		case "router":
			if routerSeen {
				return Document{}, fmt.Errorf("router config tuple %d: duplicate router section", i)
			}
			routerSeen = true
			p, err := parseEntity[entities.Router, *entities.Router](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.Router = p
		case "site":
			if result.Site != nil {
				return Document{}, fmt.Errorf("router config tuple %d: duplicate site section", i)
			}
			p, err := parseEntity[entities.Site, *entities.Site](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.Site = &p
		case "sslProfile":
			p, err := parseEntity[entities.SslProfile, *entities.SslProfile](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.SslProfiles = append(result.SslProfiles, p)
		case "proxyProfile":
			p, err := parseEntity[entities.ProxyProfile, *entities.ProxyProfile](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.ProxyProfiles = append(result.ProxyProfiles, p)
		case "listener":
			p, err := parseEntity[entities.Listener, *entities.Listener](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.Listeners = append(result.Listeners, p)
		case "connector":
			p, err := parseEntity[entities.Connector, *entities.Connector](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.Connectors = append(result.Connectors, p)
		case "address":
			p, err := parseEntity[entities.ConfigAddress, *entities.ConfigAddress](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.Addresses = append(result.Addresses, p)
		case "tcpListener":
			p, err := parseEntity[entities.TcpListener, *entities.TcpListener](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.TcpListeners = append(result.TcpListeners, p)
		case "tcpConnector":
			p, err := parseEntity[entities.TcpConnector, *entities.TcpConnector](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.TcpConnectors = append(result.TcpConnectors, p)
		case "listenerAddress":
			p, err := parseEntity[entities.ListenerAddress, *entities.ListenerAddress](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.ListenerAddresses = append(result.ListenerAddresses, p)
		case "log":
			p, err := parseEntity[entities.Log, *entities.Log](tuple[1])
			if err != nil {
				return Document{}, sectionError(i, section, err)
			}
			result.Logs = append(result.Logs, p)
		default:
			return Document{}, fmt.Errorf("router config tuple %d: unknown section %q", i, section)
		}
	}
	return result, nil
}

func sectionError(index int, section string, err error) error {
	return fmt.Errorf("router config tuple %d (%s): %w", index, section, err)
}

func parseEntity[E any, PE mgmt.Entity[E]](raw json.RawMessage) (mgmt.Partial[E], error) {
	var result mgmt.Partial[E]
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return result, fmt.Errorf("attributes must be an object")
	}
	var attributes map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&attributes); err != nil {
		return result, fmt.Errorf("attributes must be an object: %w", err)
	}
	typ := mgmt.TypeOf[E, PE]()
	for name, rawValue := range attributes {
		ordinal := -1
		for i := range typ.Attributes {
			if typ.Attributes[i].Name == name {
				ordinal = i
				break
			}
		}
		if ordinal < 0 {
			return result, fmt.Errorf("unknown attribute %q", name)
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(rawValue))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return result, fmt.Errorf("attribute %q: %w", name, err)
		}
		coerced, err := coerce(value, typ.Attributes[ordinal].Kind)
		if err != nil {
			return result, fmt.Errorf("attribute %q: %w", name, err)
		}
		field := mgmt.Field[E](ordinal)
		if err := PE(&result.Value).SetManagementField(field, coerced); err != nil {
			return result, err
		}
		result.Fields = result.Fields.Union(mgmt.Fields(field))
	}
	return result, nil
}

func coerce(value any, kind mgmt.AttributeKind) (any, error) {
	if value == nil && kind != mgmt.MapAttribute && kind != mgmt.ListAttribute {
		return nil, fmt.Errorf("attribute must not be null")
	}
	switch kind {
	case mgmt.StringAttribute:
		if n, ok := value.(json.Number); ok {
			return n.String(), nil
		}
	case mgmt.Int64Attribute:
		if s, ok := value.(string); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, err
			}
			return n, nil
		}
		if n, ok := value.(json.Number); ok {
			return n.Int64()
		}
	case mgmt.Uint64Attribute:
		if s, ok := value.(string); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, err
			}
			return n, nil
		}
		if n, ok := value.(json.Number); ok {
			return strconv.ParseUint(n.String(), 10, 64)
		}
	case mgmt.BoolAttribute:
		if s, ok := value.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "yes", "on", "1":
				return true, nil
			case "false", "no", "off", "0":
				return false, nil
			default:
				return nil, fmt.Errorf("invalid boolean %q", s)
			}
		}
		if n, ok := value.(json.Number); ok {
			if n.String() == "1" {
				return true, nil
			}
			if n.String() == "0" {
				return false, nil
			}
		}
	case mgmt.MapAttribute, mgmt.ListAttribute:
		return wireValue(value)
	}
	return value, nil
}

// JSON numbers inside openProperties must also be usable by the AMQP codec;
// json.Number itself is not an AMQP wire type.
func wireValue(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(v.String(), ".eE") {
			return v.Float64()
		}
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		return strconv.ParseUint(v.String(), 10, 64)
	case map[string]any:
		for key, item := range v {
			converted, err := wireValue(item)
			if err != nil {
				return nil, err
			}
			v[key] = converted
		}
	case []any:
		for i, item := range v {
			converted, err := wireValue(item)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
	}
	return value, nil
}

// Marshal emits deterministic skrouterd JSON. Sections and attributes use
// schema order; repeated sections are ordered by their identifying attribute.
func (d Document) Marshal() ([]byte, error) {
	var tuples []wireTuple
	if d.Router.Fields != 0 {
		v, err := marshalEntity[entities.Router, *entities.Router](d.Router)
		if err != nil {
			return nil, err
		}
		tuples = append(tuples, wireTuple{"router", v})
	}
	if d.Site != nil {
		v, err := marshalEntity[entities.Site, *entities.Site](*d.Site)
		if err != nil {
			return nil, err
		}
		tuples = append(tuples, wireTuple{"site", v})
	}
	add := func(section string, values []wireTuple) {
		sort.SliceStable(values, func(i, j int) bool {
			left, right := entityKey(values[i].value), entityKey(values[j].value)
			if left == right {
				// Unnamed entries (and equal keys) still have a canonical order.
				// Encoding errors are reported when emitting the tuples below.
				a, _ := json.Marshal(values[i].value)
				b, _ := json.Marshal(values[j].value)
				return bytes.Compare(a, b) < 0
			}
			return left < right
		})
		for _, v := range values {
			tuples = append(tuples, wireTuple{section, v.value})
		}
	}
	if v, e := marshalSlice[entities.SslProfile, *entities.SslProfile](d.SslProfiles); e != nil {
		return nil, e
	} else {
		add("sslProfile", v)
	}
	if v, e := marshalSlice[entities.ProxyProfile, *entities.ProxyProfile](d.ProxyProfiles); e != nil {
		return nil, e
	} else {
		add("proxyProfile", v)
	}
	if v, e := marshalSlice[entities.Listener, *entities.Listener](d.Listeners); e != nil {
		return nil, e
	} else {
		add("listener", v)
	}
	if v, e := marshalSlice[entities.Connector, *entities.Connector](d.Connectors); e != nil {
		return nil, e
	} else {
		add("connector", v)
	}
	if v, e := marshalSlice[entities.ConfigAddress, *entities.ConfigAddress](d.Addresses); e != nil {
		return nil, e
	} else {
		add("address", v)
	}
	if v, e := marshalSlice[entities.TcpListener, *entities.TcpListener](d.TcpListeners); e != nil {
		return nil, e
	} else {
		add("tcpListener", v)
	}
	if v, e := marshalSlice[entities.TcpConnector, *entities.TcpConnector](d.TcpConnectors); e != nil {
		return nil, e
	} else {
		add("tcpConnector", v)
	}
	if v, e := marshalSlice[entities.ListenerAddress, *entities.ListenerAddress](d.ListenerAddresses); e != nil {
		return nil, e
	} else {
		add("listenerAddress", v)
	}
	if v, e := marshalSlice[entities.Log, *entities.Log](d.Logs); e != nil {
		return nil, e
	} else {
		add("log", v)
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for i, t := range tuples {
		if i > 0 {
			out.WriteByte(',')
		}
		a, _ := json.Marshal(t.section)
		b, err := json.Marshal(t.value)
		if err != nil {
			return nil, fmt.Errorf("router config %s: %w", t.section, err)
		}
		out.WriteByte('[')
		out.Write(a)
		out.WriteByte(',')
		out.Write(b)
		out.WriteByte(']')
	}
	out.WriteByte(']')
	return out.Bytes(), nil
}

func marshalSlice[E any, PE mgmt.Entity[E]](items []mgmt.Partial[E]) ([]wireTuple, error) {
	r := make([]wireTuple, 0, len(items))
	for _, p := range items {
		v, e := marshalEntity[E, PE](p)
		if e != nil {
			return nil, e
		}
		r = append(r, wireTuple{value: v})
	}
	return r, nil
}

// orderedObject retains the management schema's attribute order in JSON.
type orderedObject []struct {
	name  string
	value any
}

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		n, _ := json.Marshal(p.name)
		v, e := json.Marshal(p.value)
		if e != nil {
			return nil, e
		}
		b.Write(n)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalEntity[E any, PE mgmt.Entity[E]](p mgmt.Partial[E]) (orderedObject, error) {
	typ := mgmt.TypeOf[E, PE]()
	valid := uint64(0)
	if len(typ.Attributes) == 64 {
		valid = ^uint64(0)
	} else {
		valid = (uint64(1) << len(typ.Attributes)) - 1
	}
	if uint64(p.Fields)&^valid != 0 {
		return nil, fmt.Errorf("%s: field mask contains unknown bits %#x", typ.Short, uint64(p.Fields)&^valid)
	}
	o := orderedObject{}
	for name, value := range mgmt.Attributes[E, PE](p) {
		o = append(o, struct {
			name  string
			value any
		}{name, value})
	}
	return o, nil
}

func entityKey(o orderedObject) string {
	for _, wanted := range []string{"name", "module", "prefix", "pattern", "address", "listener"} {
		for _, p := range o {
			if p.name == wanted {
				return fmt.Sprint(p.value)
			}
		}
	}
	return ""
}

// Equal compares documents as deterministic configurations; repeated-section
// input ordering is not significant.
func (d Document) Equal(other Document) bool {
	a, e1 := d.Marshal()
	b, e2 := other.Marshal()
	return e1 == nil && e2 == nil && bytes.Equal(a, b)
}
