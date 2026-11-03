package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// parseTree leniently parses doc into the generic tree the walk consumes.
func parseTree(t *testing.T, doc string) map[string]any {
	t.Helper()
	var tree map[string]any
	if err := toml.Unmarshal([]byte(doc), &tree); err != nil {
		t.Fatalf("lenient parse of %q: %v", doc, err)
	}
	return tree
}

// walkTree walks doc against schema and returns the collected errors.
func walkTree(t *testing.T, doc string, schema reflect.Type) []ConfigError {
	t.Helper()
	var errs []ConfigError
	walkValue("", parseTree(t, doc), schema, 0, &errs)
	return errs
}

// assertWalkError asserts errs contains exactly one error with the given
// field and message.
func assertWalkError(t *testing.T, errs []ConfigError, field, msg string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("got %d errors (%v), want 1", len(errs), errs)
	}
	if errs[0].Field != field || errs[0].Msg != msg {
		t.Fatalf("got %v, want {Field: %q, Msg: %q}", errs[0], field, msg)
	}
}

// walkBase is the embedded-struct fixture: its subfields must be promoted
// to the parent level in the walk, mirroring the decoder.
type walkBase struct {
	Timeout int    `toml:"timeout"`
	Kind    string `toml:"kind"`
}

type withBase struct {
	walkBase
	Name string `toml:"name"`
}

// TestWalk_EmbeddedStructFieldsArePromoted: keys for an untagged anonymous
// struct's subfields resolve at the parent level, so they are neither
// unknown nor hidden behind the field name, and an unknown key next to them
// is reported at the top level.
func TestWalk_EmbeddedStructFieldsArePromoted(t *testing.T) {
	t.Parallel()
	errs := walkTree(t, "timeout = 3\nkind = \"a\"\nname = \"x\"\n", reflect.TypeOf(withBase{}))
	if len(errs) != 0 {
		t.Fatalf("promoted keys must resolve, got %v", errs)
	}

	errs = walkTree(t, "timeout = 3\nbogus = 1\n", reflect.TypeOf(withBase{}))
	assertWalkError(t, errs, "bogus", "unknown field")

	// A shape error on a promoted subfield is anchored at the parent level,
	// with no extra "walkBase" segment.
	errs = walkTree(t, "timeout = \"slow\"\n", reflect.TypeOf(withBase{}))
	assertWalkError(t, errs, "timeout", "expected integer, got string")
}

// TestWalk_EmbeddedTableIsUnknownWhenPromoted pins the other half of the
// promotion contract: the decoder does not accept a [walkBase] table for an
// untagged embedded struct, so neither does the walk.
func TestWalk_EmbeddedTableIsUnknownWhenPromoted(t *testing.T) {
	t.Parallel()
	errs := walkTree(t, "[walkBase]\ntimeout = 3\n", reflect.TypeOf(withBase{}))
	assertWalkError(t, errs, "walkBase", "unknown field")
}

// TestWalk_EmbeddedPointerIsPromoted: embedded pointers to structs promote
// exactly like embedded structs.
func TestWalk_EmbeddedPointerIsPromoted(t *testing.T) {
	t.Parallel()
	type withPtrBase struct {
		*walkBase
		Name string `toml:"name"`
	}
	errs := walkTree(t, "timeout = 3\nname = \"x\"\n", reflect.TypeOf(withPtrBase{}))
	if len(errs) != 0 {
		t.Fatalf("promoted pointer keys must resolve, got %v", errs)
	}
	errs = walkTree(t, "bogus = 1\n", reflect.TypeOf(withPtrBase{}))
	assertWalkError(t, errs, "bogus", "unknown field")
}

// walkInner is the struct-valued map fixture.
type walkInner struct {
	Name string `toml:"name"`
}

// TestWalk_UnknownFieldInsideMapValue map fields have free-form keys but
// structured values: unknown keys inside a value are detected and addressed
// through the TOML-quoted map key (the Appendix B display style).
func TestWalk_UnknownFieldInsideMapValue(t *testing.T) {
	t.Parallel()
	type schema struct {
		Labels map[string]walkInner `toml:"labels"`
	}
	errs := walkTree(t, "[labels.prod]\nname = \"x\"\n", reflect.TypeOf(schema{}))
	if len(errs) != 0 {
		t.Fatalf("known map value keys must resolve, got %v", errs)
	}

	errs = walkTree(t, "[labels.prod]\nname = \"x\"\nbogus = 1\n", reflect.TypeOf(schema{}))
	assertWalkError(t, errs, `labels."prod".bogus`, "unknown field")

	// Shape errors inside map values carry the same addressing.
	errs = walkTree(t, "[labels.prod]\nname = 5\n", reflect.TypeOf(schema{}))
	assertWalkError(t, errs, `labels."prod".name`, "expected string, got integer")

	// Keys that need quoting in TOML are quoted in the path too.
	errs = walkTree(t, `[labels."jev-latest"]
name = 5
`, reflect.TypeOf(schema{}))
	assertWalkError(t, errs, `labels."jev-latest".name`, "expected string, got integer")
}

// TestWalk_NestedArrayElementsAreIndexed: unknown keys inside nested
// arrays-of-tables carry the full index chain.
func TestWalk_NestedArrayElementsAreIndexed(t *testing.T) {
	t.Parallel()
	type key struct {
		ID string `toml:"id"`
	}
	type schema struct {
		Keys []key `toml:"keys"`
	}
	errs := walkTree(t, "[[keys]]\nid = \"a\"\n\n[[keys]]\nid = \"b\"\nbogus = 1\n", reflect.TypeOf(schema{}))
	assertWalkError(t, errs, "keys[1].bogus", "unknown field")

	errs = walkTree(t, "keys = [{id = \"a\"}, {id = \"b\", bogus = 2}]\n", reflect.TypeOf(schema{}))
	assertWalkError(t, errs, "keys[1].bogus", "unknown field")
}

// TestWalk_MirrorsDecoderLeniency pins the acceptance matrix the walk must
// not over-report: integers for float fields and a single table for an
// array-of-tables field are accepted by go-toml and by the walk.
func TestWalk_MirrorsDecoderLeniency(t *testing.T) {
	t.Parallel()
	type schema struct {
		Ratio float64     `toml:"ratio"`
		Items []walkInner `toml:"items"`
	}
	errs := walkTree(t, "ratio = 1\n[items]\nname = \"x\"\n", reflect.TypeOf(schema{}))
	if len(errs) != 0 {
		t.Fatalf("lenient forms must not be reported, got %v", errs)
	}

	// But a table is not accepted for a scalar-element array.
	type scalarSchema struct {
		Names []string `toml:"names"`
	}
	errs = walkTree(t, "[names]\nx = 1\n", reflect.TypeOf(scalarSchema{}))
	assertWalkError(t, errs, "names", "expected array, got table")
}

// walkNode is a self-referential-pointer schema type: building a shadow
// type of it recursed forever; the walk must handle it.
type walkNode struct {
	Next *walkNode `toml:"next"`
	V    string    `toml:"v"`
}

// TestWalk_SelfReferentialPointerSchemaIsWalkable proves the walk survives
// a schema type that contains itself through a pointer field.
func TestWalk_SelfReferentialPointerSchemaIsWalkable(t *testing.T) {
	t.Parallel()
	errs := walkTree(t, "v = \"root\"\n[next]\nv = \"child\"\n", reflect.TypeOf(walkNode{}))
	if len(errs) != 0 {
		t.Fatalf("walk of self-referential schema failed: %v", errs)
	}
	errs = walkTree(t, "v = \"root\"\n[next]\nv = 5\n", reflect.TypeOf(walkNode{}))
	assertWalkError(t, errs, "next.v", "expected string, got integer")
}

// TestWalk_DepthGuardBoundsRunawayNesting: a document nested past the depth
// bound yields an error, not a stack overflow.
func TestWalk_DepthGuardBoundsRunawayNesting(t *testing.T) {
	t.Parallel()
	leaf := map[string]any{"v": "leaf"}
	for range maxWalkDepth + 10 {
		leaf = map[string]any{"v": "x", "next": leaf}
	}
	var errs []ConfigError
	walkValue("", leaf, reflect.TypeOf(walkNode{}), 0, &errs)
	if len(errs) == 0 {
		t.Fatal("deeply nested document produced no error")
	}
	sawDepth := false
	for _, ce := range errs {
		if strings.Contains(ce.Msg, "nested deeper than") {
			sawDepth = true
		}
	}
	if !sawDepth {
		t.Fatalf("no depth error among %v", errs)
	}
}

// TestWalk_StructFieldsFlattensEmbeddedOnce pins the cycle guard of the
// field flattening itself: a struct that embeds its own pointer type still
// yields a finite field set.
func TestWalk_StructFieldsFlattensEmbeddedOnce(t *testing.T) {
	t.Parallel()
	type cyclic struct {
		*cyclic
		V string `toml:"v"`
	}
	// Build the cycle for real — the embedded pointer points back at the
	// value — so the flattening is exercised on the shape it must guard.
	c := &cyclic{V: "x"}
	c.cyclic = c
	fields := structFields(reflect.TypeOf(*c))
	if _, ok := fields["v"]; !ok {
		t.Errorf("fields missing own key %q: %v", "v", fields)
	}
	if len(fields) != 1 {
		t.Errorf("self embedding leaked extra fields: %v", fields)
	}
}

// TestWalk_KindNames covers the kind-name rendering used in shape errors.
func TestWalk_KindNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val    any
		schema reflect.Type
		want   string
	}{
		{val: int64(3), schema: reflect.TypeOf(""), want: "expected string, got integer"},
		{val: "x", schema: reflect.TypeOf(0), want: "expected integer, got string"},
		{val: 1.5, schema: reflect.TypeOf(0), want: "expected integer, got float"},
		{val: true, schema: reflect.TypeOf(0), want: "expected integer, got boolean"},
		{val: map[string]any{}, schema: reflect.TypeOf(0), want: "expected integer, got table"},
		{val: []any{}, schema: reflect.TypeOf(0), want: "expected integer, got array"},
		{val: int64(1), schema: reflect.TypeOf([]any{}), want: "expected array, got integer"},
		{val: "x", schema: reflect.TypeOf(map[string]any{}), want: "expected table, got string"},
	}
	for _, tc := range cases {
		var errs []ConfigError
		walkValue("f", tc.val, tc.schema, 0, &errs)
		if len(errs) != 1 {
			t.Fatalf("walk of %v against %s: got %v", tc.val, tc.schema, errs)
		}
		if got := errs[0].Msg; got != tc.want {
			t.Errorf("walk of %v against %s: msg = %q, want %q", tc.val, tc.schema, got, tc.want)
		}
		if got := errs[0].Field; got != "f" {
			t.Errorf("error anchored at %q, want f", got)
		}
	}
}

// TestWalk_DatetimeValues pins datetime handling: accepted for time.Time
// schema fields, rejected where a string is expected.
func TestWalk_DatetimeValues(t *testing.T) {
	t.Parallel()
	type schema struct {
		At  time.Time `toml:"at"`
		Raw string    `toml:"raw"`
	}
	errs := walkTree(t, "at = 1979-05-27T07:32:00Z\nraw = 1979-05-27\n", reflect.TypeOf(schema{}))
	assertWalkError(t, errs, "raw", "expected string, got datetime")

	var derrs []ConfigError
	walkValue("at", parseTree(t, "at = 1979-05-27T07:32:00Z")["at"], reflect.TypeOf(time.Time{}), 0, &derrs)
	if len(derrs) != 0 {
		t.Fatalf("datetime into time.Time must be accepted, got %v", derrs)
	}
}

// walkDuration is a custom scalar: it decodes duration strings itself via
// encoding.TextUnmarshaler, the interface go-toml honours for user types.
type walkDuration struct {
	time.Duration
}

func (d *walkDuration) UnmarshalText(b []byte) error {
	parsed, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

// TestWalk_CustomScalarTypesAreAccepted: schema types whose pointer
// implements encoding.TextUnmarshaler own their decoding, so the walk
// must not kind-check them (a struct-typed field fed a string is fine).
func TestWalk_CustomScalarTypesAreAccepted(t *testing.T) {
	t.Parallel()
	type schema struct {
		Timeout walkDuration `toml:"timeout"`
	}
	errs := walkTree(t, "timeout = \"5s\"\n", reflect.TypeOf(schema{}))
	if len(errs) != 0 {
		t.Fatalf("custom scalar must not be kind-checked, got %v", errs)
	}

	// The exemption applies wherever the type sits, including inside a
	// map value.
	type entry struct {
		Duration walkDuration `toml:"duration"`
	}
	type mapSchema struct {
		Timeouts map[string]entry `toml:"timeouts"`
	}
	errs = walkTree(t, "[timeouts.prod]\nduration = \"5s\"\n", reflect.TypeOf(mapSchema{}))
	if len(errs) != 0 {
		t.Fatalf("custom scalar inside a map value must not be kind-checked, got %v", errs)
	}
}

// TestWalk_DashTagNamesFieldLiteralDash pins the tag reading: toml:"-"
// opts the field out entirely, while toml:"-," names a field literally
// called "-" (the comma is the tag separator), matching go-toml.
func TestWalk_DashTagNamesFieldLiteralDash(t *testing.T) {
	t.Parallel()
	type namedDash struct {
		Dash string `toml:"-,"`
		Kept string `toml:"kept"`
	}
	errs := walkTree(t, "- = \"x\"\nkept = \"y\"\n", reflect.TypeOf(namedDash{}))
	if len(errs) != 0 {
		t.Fatalf(`field named "-" by the "-," tag must resolve, got %v`, errs)
	}

	type skippedDash struct {
		Dash string `toml:"-"`
		Kept string `toml:"kept"`
	}
	errs = walkTree(t, "- = \"x\"\n", reflect.TypeOf(skippedDash{}))
	assertWalkError(t, errs, "-", "unknown field")
}

// TestStripIndexSegments_KeepsQuotedMapKeys pins that a bracket inside a
// quoted map key is not mistaken for an array index.
func TestStripIndexSegments_KeepsQuotedMapKeys(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"providers[0].bogus":               "providers.bogus",
		`models."a[0]".targets[1].model`:   `models."a[0]".targets.model`,
		`accounts."x[12]"`:                 `accounts."x[12]"`,
		`models."a\"[0]".targets[0].model`: `models."a\"[0]".targets.model`,
		"not-an-index[abc]":                "not-an-index[abc]",
	}
	for in, want := range cases {
		if got := stripIndexSegments(in); got != want {
			t.Errorf("stripIndexSegments(%q) = %q, want %q", in, got, want)
		}
	}
}
