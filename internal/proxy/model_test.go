package proxy

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestFindModel_LocatesTheTopLevelValue pins the scan contract, including
// the byte span a rewrite will splice.
func TestFindModel_LocatesTheTopLevelValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"only member", `{"model":"jev-latest"}`, "jev-latest"},
		{"first member", `{"model":"a","state":"x"}`, "a"},
		{"last member", `{"state":"x","model":"a"}`, "a"},
		{"nested ignored", `{"questions":{"model":"nested"},"model":"top"}`, "top"},
		{"array ignored", `{"list":[{"model":"nested"}],"model":"top"}`, "top"},
		{"whitespace", "{\n  \"model\" : \"a b\"\n}", "a b"},
		{"escaped value", `{"model":"a\u002fb"}`, "a/b"},
		{"escaped key counts", `{"mod\u0065l":"a"}`, "a"},
		{"string with braces", `{"state":"{ not json }","model":"a"}`, "a"},
		{"string with quote", `{"state":"he said \"hi\"","model":"a"}`, "a"},
		{"empty model", `{"model":""}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			span, err := FindModel([]byte(tc.body))
			if err != nil {
				t.Fatalf("FindModel: %v", err)
			}
			if span.Value != tc.want {
				t.Fatalf("Value = %q, want %q", span.Value, tc.want)
			}
			// The span must delimit exactly the quoted value.
			var decoded string
			if err := json.Unmarshal([]byte(tc.body)[span.Start:span.End], &decoded); err != nil {
				t.Fatalf("span %d:%d is not a JSON string: %v", span.Start, span.End, err)
			}
			if decoded != tc.want {
				t.Fatalf("span decoded to %q, want %q", decoded, tc.want)
			}
		})
	}
}

// TestFindModel_RejectsAmbiguousBodies pins the cases that must not be
// routed: no model, duplicates, and non-objects.
func TestFindModel_RejectsAmbiguousBodies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want error
	}{
		{"empty", ``, ErrMalformedBody},
		{"array", `["model"]`, ErrMalformedBody},
		{"string", `"model"`, ErrMalformedBody},
		{"missing", `{"state":"x"}`, ErrMissingModel},
		{"nested only", `{"q":{"model":"x"}}`, ErrMissingModel},
		{"non string", `{"model":42}`, ErrMissingModel},
		{"null", `{"model":null}`, ErrMissingModel},
		{"duplicate", `{"model":"a","model":"b"}`, ErrDuplicateModel},
		{"duplicate escaped", `{"model":"a","mod\u0065l":"b"}`, ErrDuplicateModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := FindModel([]byte(tc.body)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// FuzzFindModel asserts the scanner never panics, always returns a span
// that lies inside the body, and never reports success on a body that is
// not a JSON object containing a string model.
func FuzzFindModel(f *testing.F) {
	f.Add([]byte(`{"model":"jev-latest","state":"x"}`))
	f.Add([]byte(`{"model":""}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`{"a":{"b":[1,2,{"c":"d"}]},"model":"m"}`))
	f.Add([]byte(`{"model":"\u0000"}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		span, err := FindModel(body)
		if err != nil {
			return
		}
		if span.Start < 0 || span.End > len(body) || span.Start >= span.End {
			t.Fatalf("span %d:%d outside body of length %d", span.Start, span.End, len(body))
		}
		if body[span.Start] != '"' || body[span.End-1] != '"' {
			t.Fatalf("span does not cover a quoted value: %q", body[span.Start:span.End])
		}
		var decoded string
		if err := json.Unmarshal(body[span.Start:span.End], &decoded); err != nil {
			t.Fatalf("span is not a JSON string: %v", err)
		}
		if decoded != span.Value {
			t.Fatalf("Value = %q but span decodes to %q", span.Value, decoded)
		}
		// An accepted body must be a JSON object with a top-level model.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(body, &probe); err != nil {
			t.Fatalf("accepted a body that is not a JSON object: %v", err)
		}
		if _, ok := probe["model"]; !ok {
			t.Fatalf("accepted a body without a top-level model")
		}
	})
}
