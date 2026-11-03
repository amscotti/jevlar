package config

import (
	"encoding"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// The document walk is the single source of validation-style decode errors.
// The TOML document is first parsed leniently into a generic tree
// (map[string]any) — which cannot fail on unknown fields or shape problems —
// and then walked against the schema type by reflection. The walk mirrors
// the strict decoder's leniencies (integers for float fields, a single
// [table] for an array-of-tables field, promoted embedded-struct keys),
// with one deliberate divergence: keys are matched case-sensitively, so
// `[Server]` or `Config_Version` is reported as an unknown field even
// though go-toml's decoder case-folds key matching. The strict spelling is
// the only accepted one (see ADR 0002). The walk reports:
//
//   - every unknown-field path, dotted and index-aware, so a document with
//     several problems reports them all even though the real decoder
//     abandons at the first one;
//   - every shape mismatch (integer where a string is expected, table where
//     an array-of-tables is expected, and so on) as
//     "expected <kind>, got <toml kind>".
//
// The real strict decode still runs for struct population; when it fails on
// a problem the walk already reported, its error is dropped so nothing is
// reported twice (see decodeErrors).

// msgUnknownField is the message for unknown-field errors.
const msgUnknownField = "unknown field"

// maxWalkDepth bounds the recursive walk. Documents this nested are not
// configuration; the guard also protects the walk against pathological
// (including schema-driven) cycles.
const maxWalkDepth = 64

// timeType anchors datetime recognition for schema fields.
var timeType = reflect.TypeOf(time.Time{})

// textUnmarshalerType anchors the custom-scalar interface: a schema type
// whose pointer implements encoding.TextUnmarshaler owns its own decoding
// (go-toml decodes such types from a TOML string), so the walk cannot know
// what they accept. go-toml's other unmarshaler interface is unstable and
// not enabled by this package.
var textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

// walkValue checks val against schema type t, appending one ConfigError per
// unknown field or shape mismatch found at or below path. Pointer schema
// types are transparent. depth guards against runaway recursion.
func walkValue(path string, val any, t reflect.Type, depth int, errs *[]ConfigError) {
	if depth > maxWalkDepth {
		*errs = append(*errs, ConfigError{
			Field: path,
			Msg:   fmt.Sprintf("configuration nested deeper than %d levels", maxWalkDepth),
		})
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		if !isTomlDatetime(val) {
			reportMismatch(path, t, val, errs)
		}
	case hasCustomUnmarshaler(t):
		// Custom scalar types own their decoding (encoding.TextUnmarshaler
		// on the pointer); kind checking does not apply to them.
	case t.Kind() == reflect.Struct:
		walkTable(path, val, t, depth, errs)
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		walkArray(path, val, t, depth, errs)
	case t.Kind() == reflect.Map:
		walkMapTable(path, val, t, depth, errs)
	case t.Kind() == reflect.String:
		if _, ok := val.(string); !ok {
			reportMismatch(path, t, val, errs)
		}
	case t.Kind() == reflect.Bool:
		if _, ok := val.(bool); !ok {
			reportMismatch(path, t, val, errs)
		}
	case isIntKind(t.Kind()):
		if _, ok := val.(int64); !ok {
			reportMismatch(path, t, val, errs)
		}
	case isFloatKind(t.Kind()):
		switch val.(type) {
		case int64, float64:
			// The decoder accepts integers for float fields.
		default:
			reportMismatch(path, t, val, errs)
		}
	case t.Kind() == reflect.Interface:
		// Any-typed fields accept every TOML value; nothing to check.
	default:
		// Kinds TOML cannot deliver (channels, funcs, complex numbers);
		// reported rather than silently accepted.
		reportMismatch(path, t, val, errs)
	}
}

// walkTable checks a TOML table against a struct schema type: every
// document key must match a schema field (unknown keys are reported at
// their dotted path) and every matched value is walked against its field
// type.
func walkTable(path string, val any, t reflect.Type, depth int, errs *[]ConfigError) {
	table, ok := val.(map[string]any)
	if !ok {
		reportMismatch(path, t, val, errs)
		return
	}
	fields := structFields(t)
	for _, key := range sortedKeys(table) {
		child := joinField(path, displayKey(key))
		ft, known := fields[key]
		if !known {
			*errs = append(*errs, ConfigError{Field: child, Msg: msgUnknownField})
			continue
		}
		walkValue(child, table[key], ft, depth+1, errs)
	}
}

// walkArray checks a TOML array against a slice or array schema type,
// addressing elements as path[i]. A single [table] is accepted as a
// one-element array when the element type is itself table-shaped, mirroring
// the decoder's leniency; anything else non-array is a mismatch.
func walkArray(path string, val any, t reflect.Type, depth int, errs *[]ConfigError) {
	elem := t.Elem()
	switch av := val.(type) {
	case []any:
		for i, el := range av {
			walkValue(fmt.Sprintf("%s[%d]", path, i), el, elem, depth+1, errs)
		}
	case map[string]any:
		if acceptsTable(elem) {
			walkValue(path+"[0]", av, elem, depth+1, errs)
		} else {
			reportMismatch(path, t, val, errs)
		}
	default:
		reportMismatch(path, t, val, errs)
	}
}

// walkMapTable checks a TOML table against a map[string]X schema type: the
// keys are free-form configuration data and are addressed with TOML
// quoting (labels."prod".name, the Appendix B display style); every value
// is walked against X.
func walkMapTable(path string, val any, t reflect.Type, depth int, errs *[]ConfigError) {
	table, ok := val.(map[string]any)
	if !ok {
		reportMismatch(path, t, val, errs)
		return
	}
	for _, key := range sortedKeys(table) {
		walkValue(joinField(path, quoteTomlKey(key)), table[key], t.Elem(), depth+1, errs)
	}
}

// quoteTomlKey renders a free-form map key as a TOML basic string, the
// display style for map-key segments of a field path (models."jev-latest")
// regardless of whether quoting is required.
func quoteTomlKey(key string) string {
	return strconv.Quote(key)
}

// displayKey renders a document key as a field-path segment. The empty key
// — TOML permits "" = 1 — renders as <empty> so a path never dangles:
// server.<empty>: unknown field, not "server.".
func displayKey(key string) string {
	if key == "" {
		return "<empty>"
	}
	return key
}

// hasCustomUnmarshaler reports whether values of schema type t own their
// decoding: the pointer type implements encoding.TextUnmarshaler.
func hasCustomUnmarshaler(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(textUnmarshalerType)
}

// structFields flattens the TOML-visible fields of struct type t into a map
// from configuration key to field type. Anonymous struct and
// pointer-to-struct fields without a toml tag are promoted: their subfields
// appear at the parent level, mirroring the decoder's embedded-field
// handling. An anonymous field with a toml tag is an ordinary named field.
// Cyclic embedding chains terminate: each struct type is expanded once.
func structFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	addStructFields(fields, t, map[reflect.Type]bool{})
	return fields
}

// addStructFields adds t's fields to fields; visited tracks struct types
// already expanded so embedding cycles (a struct embedding itself through
// a pointer) cannot recurse forever.
func addStructFields(fields map[string]reflect.Type, t reflect.Type, visited map[reflect.Type]bool) {
	if visited[t] {
		return
	}
	visited[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported fields are invisible to TOML
		}
		// The exact tag "-" opts the field out of TOML entirely; "-," names
		// a field literally called "-" (the comma is the tag separator),
		// matching go-toml's reading of the tag.
		if f.Tag.Get("toml") == "-" {
			continue
		}
		name := tomlName(f)
		if f.Anonymous && name == f.Name && isStructType(f.Type) {
			addStructFields(fields, deref(f.Type), visited)
			continue
		}
		if f.PkgPath != "" {
			// Unexported anonymous non-struct: not promotable and not
			// addressable by the decoder either.
			continue
		}
		fields[name] = f.Type
	}
}

// isStructType reports whether t is a struct or a pointer to one, the field
// shapes that embed promotion applies to.
func isStructType(t reflect.Type) bool {
	return deref(t).Kind() == reflect.Struct && deref(t) != timeType
}

// deref unwraps pointer types.
func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// acceptsTable reports whether a document table can decode into schema
// type t (after pointer unwrapping): struct, map and any-typed fields can.
func acceptsTable(t reflect.Type) bool {
	k := deref(t).Kind()
	return k == reflect.Struct || k == reflect.Map || k == reflect.Interface
}

// reportMismatch appends a shape error: the TOML kind of val does not fit
// schema type t.
func reportMismatch(path string, t reflect.Type, val any, errs *[]ConfigError) {
	*errs = append(*errs, ConfigError{
		Field: path,
		Msg:   fmt.Sprintf("expected %s, got %s", schemaKindName(t), tomlKindName(val)),
	})
}

// schemaKindName names the TOML kind a schema type expects.
func schemaKindName(t reflect.Type) string {
	t = deref(t)
	switch {
	case t == timeType:
		return "datetime"
	case t.Kind() == reflect.Struct || t.Kind() == reflect.Map:
		return "table"
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		return "array"
	case t.Kind() == reflect.String:
		return "string"
	case t.Kind() == reflect.Bool:
		return "boolean"
	case isIntKind(t.Kind()):
		return "integer"
	case isFloatKind(t.Kind()):
		return "float"
	default:
		return t.Kind().String()
	}
}

// tomlKindName names the TOML kind of a value from a leniently parsed
// document.
func tomlKindName(val any) string {
	switch v := val.(type) {
	case nil:
		return "null" // defensive; TOML has no null
	case string:
		return "string"
	case bool:
		return "boolean"
	case int64, int, int32:
		return "integer"
	case float64:
		return "float"
	case map[string]any:
		return "table"
	case []any:
		return "array"
	case time.Time, toml.LocalDate, toml.LocalTime, toml.LocalDateTime:
		return "datetime"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// isTomlDatetime reports whether val is one of the datetime shapes go-toml
// produces for date and time literals.
func isTomlDatetime(val any) bool {
	switch val.(type) {
	case time.Time, toml.LocalDate, toml.LocalTime, toml.LocalDateTime:
		return true
	default:
		return false
	}
}

// isIntKind reports whether k is one of Go's integer kinds.
func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

// isFloatKind reports whether k is one of Go's float kinds.
func isFloatKind(k reflect.Kind) bool {
	return k == reflect.Float32 || k == reflect.Float64
}

// sortedKeys returns the map's keys in sorted order so error order is
// deterministic.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stripIndexSegments removes [i] array-index segments from a walk field
// path so it can be compared with decoder key paths, which never carry
// indexes: "providers[0].bogus" becomes "providers.bogus". Map-key
// segments are kept — the decoder's key path includes map keys, and a
// numeric-looking bracket inside a quoted key ("a[0]") is part of the key,
// not an index — and any non-numeric bracket segment is kept verbatim
// (defensive; the walk no longer produces it).
func stripIndexSegments(path string) string {
	var b strings.Builder
	inQuotes := false
	for i := 0; i < len(path); i++ {
		c := path[i]
		if inQuotes && c == '\\' && i+1 < len(path) {
			b.WriteByte(c)
			i++
			b.WriteByte(path[i])
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			b.WriteByte(c)
			continue
		}
		if c != '[' || inQuotes {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(path) && path[j] != ']' {
			j++
		}
		if j < len(path) && isDigits(path[i+1:j]) {
			i = j // drop the numeric index segment
			continue
		}
		b.WriteByte(path[i])
	}
	return b.String()
}
