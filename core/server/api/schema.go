package api

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

// Schema is the JSON Schema subset the OpenAPI description needs. OpenAPI 3.1
// schemas are JSON Schema proper, so there is nothing OpenAPI-specific here.
type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Type                 string             `json:"type,omitempty"`
	Format               string             `json:"format,omitempty"`
	Description          string             `json:"description,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	ContentEncoding      string             `json:"contentEncoding,omitempty"`
}

// schemaBuilder turns Go types into schemas and collects the named ones as
// components.
//
// Generating from the types rather than writing schemas by hand is the whole
// point: a field added to a response struct and forgotten in a hand-written
// document is exactly the drift this wave exists to prevent, and no test can
// catch what nobody wrote down. The cost is that the description says only what
// the types say - a Go type carries no prose - so the prose lives on the
// endpoint declarations instead.
type schemaBuilder struct {
	defs map[string]*Schema
}

func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{defs: map[string]*Schema{}}
}

var timeType = reflect.TypeOf(time.Time{})

// For returns the schema of a value's type. A nil value has no schema.
func (b *schemaBuilder) For(v any) *Schema {
	if v == nil {
		return nil
	}
	return b.forType(reflect.TypeOf(v))
}

func (b *schemaBuilder) forType(t reflect.Type) *Schema {
	if t == nil {
		return &Schema{}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return &Schema{Type: "string", Format: "date-time"}
	}

	switch t.Kind() {
	case reflect.Struct:
		// Only struct types become components. A named string or map type
		// (event.ID, http.Header) would add a component that says nothing its
		// use site does not, so those are inlined.
		name := componentName(t)
		if name == "" {
			return b.structSchema(t)
		}
		if _, seen := b.defs[name]; !seen {
			// Reserve the name before recursing: a type that contains itself
			// would otherwise recurse forever.
			b.defs[name] = &Schema{Type: "object"}
			b.defs[name] = b.structSchema(t)
		}
		return &Schema{Ref: "#/components/schemas/" + name}

	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// Bytes go over the wire base64-encoded, which is what
			// encoding/json does with a []byte.
			return &Schema{Type: "string", ContentEncoding: "base64"}
		}
		return &Schema{Type: "array", Items: b.forType(t.Elem())}

	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: b.forType(t.Elem())}

	case reflect.Interface:
		// any: no constraint at all, which is honest for Payload and Meta.
		return &Schema{}

	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.String:
		return &Schema{Type: "string"}
	default:
		return &Schema{}
	}
}

func (b *schemaBuilder) structSchema(t reflect.Type) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	b.fields(t, s)
	if len(s.Properties) == 0 {
		s.Properties = nil
	}
	sort.Strings(s.Required)
	return s
}

// fields walks a struct, following embedded ones, because encoding/json
// inlines their fields and the schema has to say the same thing. That is not a
// detail here: the event envelope is an embedded *event.Event plus a url.
func (b *schemaBuilder) fields(t reflect.Type, into *Schema) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")

		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				b.fields(ft, into)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		into.Properties[name] = b.forType(f.Type)
		if !strings.Contains(opts, "omitempty") {
			into.Required = append(into.Required, name)
		}
	}
}

// componentName is the package-qualified type name, so chat.MessageEnvelope and
// sms.MessageEnvelope stay two different components.
func componentName(t reflect.Type) string {
	if t.Name() == "" || t.PkgPath() == "" {
		return "" // anonymous struct: inline it
	}
	pkg := t.PkgPath()
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		pkg = pkg[i+1:]
	}
	return pkg + "." + t.Name()
}

// Components returns the named schemas collected so far.
func (b *schemaBuilder) Components() map[string]*Schema { return b.defs }
