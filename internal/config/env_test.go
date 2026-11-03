package config

import (
	"errors"
	"strings"
	"testing"
)

// fakeEnv builds a Lookup over a fixed map; tests never touch the process
// environment.
func fakeEnv(vars map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestExpand_RequiredVariableIsReplaced(t *testing.T) {
	t.Parallel()
	got, err := Expand("http://${HOST}:8080", fakeEnv(map[string]string{"HOST": "example.internal"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "http://example.internal:8080"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_RequiredVariableUnsetIsErrorWithVariableName(t *testing.T) {
	t.Parallel()
	_, err := Expand("${JEVLAR_MISSING}", fakeEnv(nil))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if want := `variable "JEVLAR_MISSING" is not set`; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

func TestExpand_RequiredVariableEmptyIsErrorWithVariableName(t *testing.T) {
	t.Parallel()
	_, err := Expand("${EMPTY}", fakeEnv(map[string]string{"EMPTY": ""}))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if want := `variable "EMPTY" is empty`; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

func TestExpand_VariableWithDefaultUsesDefaultWhenUnset(t *testing.T) {
	t.Parallel()
	got, err := Expand("${HOST:-127.0.0.1}:8080", fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "127.0.0.1:8080"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_VariableWithDefaultUsesDefaultWhenEmpty(t *testing.T) {
	t.Parallel()
	got, err := Expand("${HOST:-127.0.0.1}", fakeEnv(map[string]string{"HOST": ""}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "127.0.0.1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_VariableWithDefaultUsesValueWhenSet(t *testing.T) {
	t.Parallel()
	got, err := Expand("${HOST:-127.0.0.1}", fakeEnv(map[string]string{"HOST": "10.0.0.4"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "10.0.0.4"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_VariableWithEmptyDefaultIsAllowed(t *testing.T) {
	t.Parallel()
	got, err := Expand("v${SUFFIX:-}", fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "v"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_EscapedDollarYieldsLiteralDollar(t *testing.T) {
	t.Parallel()
	got, err := Expand("$$HOME and $$${KEEP}", fakeEnv(map[string]string{"KEEP": "x"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "$HOME and $x"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_LiteralDollarWithoutBracesIsPreserved(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"$HOME":      "$HOME",
		"cost is $5": "cost is $5",
		"trailing $": "trailing $",
		"a $$ b":     "a $ b",
		"$ {}":       "$ {}",
	} {
		got, err := Expand(in, fakeEnv(nil))
		if err != nil {
			t.Fatalf("Expand(%q): unexpected error: %v", in, err)
		}
		if got != want {
			t.Fatalf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpand_UnterminatedReferenceIsErrorWithVariableName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"${FOO":           `unterminated variable reference "FOO"`,
		"pre ${FOO":       `unterminated variable reference "FOO"`,
		"${FOO:-fallback": `unterminated variable reference "FOO"`,
		"${":              `unterminated variable reference (empty name)`,
	} {
		_, err := Expand(in, fakeEnv(nil))
		if err == nil {
			t.Fatalf("Expand(%q): expected an error, got nil", in)
		}
		if err.Error() != want {
			t.Fatalf("Expand(%q) error = %q, want %q", in, err.Error(), want)
		}
	}
}

func TestExpand_EmptyBracesAreRejected(t *testing.T) {
	t.Parallel()
	_, err := Expand("${}", fakeEnv(nil))
	if err == nil || !strings.Contains(err.Error(), "empty variable name") {
		t.Fatalf("expected empty-name error, got %v", err)
	}
}

func TestExpand_InvalidVariableNameIsRejected(t *testing.T) {
	t.Parallel()
	_, err := Expand("${BAD NAME}", fakeEnv(nil))
	if err == nil || !strings.Contains(err.Error(), `invalid variable name "BAD NAME"`) {
		t.Fatalf("expected invalid-name error, got %v", err)
	}
}

func TestExpand_MultipleReferencesExpandInOnePass(t *testing.T) {
	t.Parallel()
	got, err := Expand("${SCHEME}://${HOST}:${PORT}${PATH:-/x}",
		fakeEnv(map[string]string{"SCHEME": "http", "HOST": "h", "PORT": "1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "http://h:1/x"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpand_ValueContainingDollarIsNotReExpanded(t *testing.T) {
	t.Parallel()
	got, err := Expand("${V}", fakeEnv(map[string]string{"V": "${OTHER}"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "${OTHER}"; got != want {
		t.Fatalf("got %q, want %q (variable values must not be re-expanded)", got, want)
	}
}

func TestExpand_NestedReferencesAreRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		env  map[string]string
	}{
		// The default contains a nested reference; rejected whether the
		// outer variable is set (its value would win) or unset (the default
		// would be copied through with the fragment intact).
		{name: "default nested, outer set", in: "${A:-${B}}", env: map[string]string{"A": "a", "B": "b"}},
		{name: "default nested, outer unset", in: "${A:-${B}}", env: nil},
		// The name itself contains a reference.
		{name: "nested in name", in: "${${B}:-x}", env: map[string]string{"B": "b"}},
		// Unterminated references whose remainder hides another "${".
		{name: "nested and unterminated", in: "${A:-${B", env: nil},
		{name: "nested and unterminated, outer set", in: "${A:-${B", env: map[string]string{"A": "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Expand(tc.in, fakeEnv(tc.env))
			if err == nil {
				t.Fatalf("Expand(%q) = %q, nil; want the nested-reference error", tc.in, got)
			}
			var ce ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("Expand(%q) error %v is not a ConfigError", tc.in, err)
			}
			if want := "nested ${...} references are not supported"; ce.Msg != want {
				t.Fatalf("Expand(%q) error = %q, want %q", tc.in, ce.Msg, want)
			}
		})
	}
}

// TestExpand_CloseBracesAreOrdinaryData pins the removal of the
// saw-reference heuristic: a "}" outside a reference is legal data
// wherever it appears, so brace-bearing values expand and round-trip
// exactly.
func TestExpand_CloseBracesAreOrdinaryData(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		env  map[string]string
		want string
	}{
		{name: "brace directly after reference", in: "x${V}y}", env: map[string]string{"V": "val"}, want: "xvaly}"},
		{name: "brace right after empty default, set", in: "${A:-}}", env: map[string]string{"A": "a"}, want: "a}"},
		{name: "brace right after empty default, empty-set", in: "${A:-}}", env: map[string]string{"A": ""}, want: "}"},
		{name: "brace right after empty default, unset", in: "${A:-}}", env: nil, want: "}"},
		{name: "brace later in string, set", in: "${A:-x}y}", env: map[string]string{"A": "a"}, want: "ay}"},
		{name: "brace later in string, unset", in: "${A:-x}y}", env: nil, want: "xy}"},
		// A JSON-ish value with a reference and braces round-trips exactly.
		{
			name: "json-ish value round-trips",
			in:   `{"a": "${V}", "b": {"c": "}"}}`,
			env:  map[string]string{"V": "x"},
			want: `{"a": "x", "b": {"c": "}"}}`,
		},
		{
			name: "json-ish value with default round-trips",
			in:   `{"a": "${V:-d}"}`,
			env:  nil,
			want: `{"a": "d"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Expand(tc.in, fakeEnv(tc.env))
			if err != nil {
				t.Fatalf("Expand(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpand_EscapedDollarsInDefaultsAreUnescaped: defaults unescape $$ to
// $ for grammar uniformity, exactly like the literal parts of the value.
func TestExpand_EscapedDollarsInDefaultsAreUnescaped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		env  map[string]string
		want string
	}{
		{name: "bare dollar", in: "${X:-$$}", env: nil, want: "$"},
		{name: "dollar inside default", in: "${X:-a$$b}", env: nil, want: "a$b"},
		{name: "default unused when set", in: "${X:-$$}", env: map[string]string{"X": "v"}, want: "v"},
		{name: "single dollar stays literal", in: "${X:-a$b}", env: nil, want: "a$b"},
		{name: "odd run unescapes pairwise", in: "${X:-$$$}", env: nil, want: "$$"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Expand(tc.in, fakeEnv(tc.env))
			if err != nil {
				t.Fatalf("Expand(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpand_LiteralCloseBraceBeforeAnyReferenceStaysLiteral pins the
// same for a "}" that appears before any reference.
func TestExpand_LiteralCloseBraceBeforeAnyReferenceStaysLiteral(t *testing.T) {
	t.Parallel()
	got, err := Expand("x}y${KEEP}", fakeEnv(map[string]string{"KEEP": "k"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "x}yk"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func FuzzExpand(f *testing.F) {
	f.Add("$$")
	f.Add("${A:-b}")
	f.Add("pre ${X:-$$} post")
	f.Add("${UNTERMINATED")
	f.Add("$5")
	f.Add("a$$b${C}c")
	f.Add("${}")
	f.Add("${A:-${B}}")
	f.Add("")
	f.Add("$")
	lookup := fakeEnv(map[string]string{"A": "alpha", "C": "cee", "V": "${OTHER}"})
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Expand(s, lookup)
		if err != nil {
			return
		}
		// Expansion must be deterministic: same input, same output.
		again, err2 := Expand(s, lookup)
		if err2 != nil {
			t.Fatalf("first call succeeded, second failed: %v", err2)
		}
		if got != again {
			t.Fatalf("non-deterministic expansion: %q vs %q", got, again)
		}
	})
}

// asConfigErrors extracts every ConfigError part of err.
func asConfigErrors(t *testing.T, err error) []ConfigError {
	t.Helper()
	if err == nil {
		return nil
	}
	var verrs ValidationErrors
	if errors.As(err, &verrs) {
		return verrs
	}
	var ce ConfigError
	if errors.As(err, &ce) {
		return []ConfigError{ce}
	}
	t.Fatalf("error %v contains no ConfigError", err)
	return nil
}

// TestExpand_EscapedDollarBeforeBraceSpellsReferenceLiterally: $$ consumes
// the first dollar, so $${A:-$$} expands to the literal ${A:-$} with no
// lookup — the way to write reference-looking text in a value.
func TestExpand_EscapedDollarBeforeBraceSpellsReferenceLiterally(t *testing.T) {
	t.Parallel()
	got, err := Expand(`$${A:-$$}`, fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `${A:-$}` {
		t.Fatalf("Expand = %q, want ${A:-$}", got)
	}
}
