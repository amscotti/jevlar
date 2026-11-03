package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

// restoreModel is the inverse of RewriteModel: it locates the model value
// in an already-rewritten body and puts original back. The byte-integrity
// invariant is asserted as "replacing the rewritten value reproduces the
// original bytes".
func restoreModel(rewritten []byte, original string) ([]byte, error) {
	span, err := FindModel(rewritten)
	if err != nil {
		return nil, err
	}
	return RewriteModel(rewritten, span, original)
}

// TestRewriteModel_SplicesOnlyTheModelValue pins the single-splice
// invariant: everything outside the model value is byte-identical, and
// restoring the original value reproduces the original bytes exactly.
func TestRewriteModel_SplicesOnlyTheModelValue(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"jev-latest","state":"a customer cannot pay","questions":{"team":{"type":"choice","criteria":{"billing":"Payments"}}}}`)
	span, err := FindModel(body)
	if err != nil {
		t.Fatalf("FindModel: %v", err)
	}

	rewritten, err := RewriteModel(body, span, "nimble")
	if err != nil {
		t.Fatalf("RewriteModel: %v", err)
	}
	if !bytes.Contains(rewritten, []byte(`"model":"nimble"`)) {
		t.Fatalf("rewritten body = %s", rewritten)
	}
	// The prefix before the value and the suffix after it are untouched.
	if !bytes.Equal(rewritten[:span.Start], body[:span.Start]) {
		t.Fatal("bytes before the model value changed")
	}
	suffix := len(body) - span.End
	if !bytes.Equal(rewritten[len(rewritten)-suffix:], body[span.End:]) {
		t.Fatal("bytes after the model value changed")
	}

	// Restoring the original value reproduces the original bytes exactly.
	restored, err := restoreModel(rewritten, "jev-latest")
	if err != nil {
		t.Fatalf("restoreModel: %v", err)
	}
	if !bytes.Equal(restored, body) {
		t.Fatalf("restored body differs\n got: %s\nwant: %s", restored, body)
	}
	if sha256.Sum256(restored) != sha256.Sum256(body) {
		t.Fatal("hash restoration failed")
	}
}

// TestRewriteModel_DoesNotMutateTheInput pins that attempts share one
// buffer safely: a rewrite must allocate a new slice.
func TestRewriteModel_DoesNotMutateTheInput(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"a","state":"x"}`)
	original := append([]byte(nil), body...)
	span, err := FindModel(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RewriteModel(body, span, "b-model"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, original) {
		t.Fatalf("input mutated: %s", body)
	}
}

// TestRewriteModel_RejectsUnsafeIDs pins the fail-closed charset check.
func TestRewriteModel_RejectsUnsafeIDs(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"a","state":"x"}`)
	span, _ := FindModel(body)
	for _, id := range []string{"", "has space", "quote\"", "back\\slash", strings.Repeat("a", 300), "newline\n"} {
		if _, err := RewriteModel(body, span, id); err == nil {
			t.Fatalf("rewrite accepted unsafe id %q", id)
		}
	}
	for _, id := range []string{"jev-latest", "jev-1.14.0", "tev1:0.8b", "a_b-c:d.e"} {
		if _, err := RewriteModel(body, span, id); err != nil {
			t.Fatalf("rewrite rejected safe id %q: %v", id, err)
		}
	}
}

// TestRewriteModel_KeepsJSONValid pins that the rewritten document still
// parses and carries the new model.
func TestRewriteModel_KeepsJSONValid(t *testing.T) {
	t.Parallel()
	body := []byte("{\n  \"state\" : \"x\" ,\n  \"model\"\t: \"alias\" ,\n  \"questions\": {}\n}")
	span, err := FindModel(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RewriteModel(body, span, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Model string `json:"model"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v (%s)", err, out)
	}
	if decoded.Model != "real-model" || decoded.State != "x" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// FuzzRewriteModel asserts the rewrite never panics and always preserves
// every byte outside the spliced value.
func FuzzRewriteModel(f *testing.F) {
	f.Add([]byte(`{"model":"jev-latest","state":"x"}`), "nimble")
	f.Add([]byte(`{"model":"a"}`), "b")
	f.Add([]byte(`{"model":"","questions":{"q":{"type":"noul"}}}`), "m")
	f.Add([]byte("{\n\t\"model\" : \"a\"\n}"), "z")

	f.Fuzz(func(t *testing.T, body []byte, model string) {
		span, err := FindModel(body)
		if err != nil {
			return
		}
		out, err := RewriteModel(body, span, model)
		if err != nil {
			return // rejected ids and spans are allowed
		}
		if !json.Valid(out) {
			t.Fatalf("rewrite produced invalid JSON:\n in: %s\nout: %s", body, out)
		}
		// Prefix and suffix must be untouched.
		if !bytes.Equal(out[:span.Start], body[:span.Start]) {
			t.Fatal("prefix changed")
		}
		suffix := len(body) - span.End
		if !bytes.Equal(out[len(out)-suffix:], body[span.End:]) {
			t.Fatal("suffix changed")
		}
		// The model value is exactly the requested one.
		got, err := FindModel(out)
		if err != nil {
			t.Fatalf("rewritten body has no model: %v", err)
		}
		if got.Value != model {
			t.Fatalf("model = %q, want %q", got.Value, model)
		}
	})
}
