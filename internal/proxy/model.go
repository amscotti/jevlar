package proxy

import (
	"encoding/json"
	"fmt"
)

// ModelSpan is the location of the top-level "model" value in a request
// body. Start and End delimit the value's bytes *including* the
// surrounding quotes, so a rewrite can splice a replacement in place and
// leave every other byte untouched.
type ModelSpan struct {
	// Value is the decoded model id (JSON escapes resolved).
	Value string
	// Start is the byte offset of the opening quote.
	Start int
	// End is the byte offset just past the closing quote.
	End int
}

// ErrMissingModel reports a body with no top-level "model" string.
var ErrMissingModel = fmt.Errorf("request body has no top-level \"model\" string")

// ErrDuplicateModel reports a body with more than one top-level "model"
// key. Providers disagree on which duplicate wins (most take the last),
// so a gateway that picked differently would route a request for a model
// the provider never saw; Jevlar refuses instead.
var ErrDuplicateModel = fmt.Errorf("request body has more than one top-level \"model\" key")

// ErrMalformedBody reports a body that is not a JSON object.
var ErrMalformedBody = fmt.Errorf("request body is not a JSON object")

// FindModel locates the top-level "model" member of a JSON object without
// unmarshalling the document. Only depth-1 keys named exactly "model"
// count; a nested "model" (for example inside "questions") is ignored.
// Keys are compared after escape decoding, so "mod\u0065l" is a duplicate
// of "model" rather than an invisible second key.
func FindModel(body []byte) (ModelSpan, error) {
	// The document must be a complete, valid JSON value before the scan
	// trusts any structural conclusion it draws from it. json.Valid is a
	// single allocation-free pass, and it makes "accepted implies
	// well-formed" true regardless of how lenient the scan below is about
	// trailing commas or truncation.
	if !json.Valid(body) {
		return ModelSpan{}, ErrMalformedBody
	}
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return ModelSpan{}, ErrMalformedBody
	}
	i++
	depth := 1

	var found ModelSpan
	seen := false

	for i < len(body) && depth > 0 {
		i = skipSpace(body, i)
		if i >= len(body) {
			break
		}
		switch c := body[i]; c {
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			i++
		case ',', ':':
			i++
		case '"':
			end, err := scanStringEnd(body, i)
			if err != nil {
				return ModelSpan{}, ErrMalformedBody
			}
			if depth != 1 {
				// A string nested below the top level is never a key we
				// route on.
				i = end
				continue
			}
			// At depth 1 a string immediately followed by ':' is a key;
			// anything else is an unrelated value.
			colon := skipSpace(body, end)
			if colon >= len(body) || body[colon] != ':' {
				i = end
				continue
			}
			key, err := decodeJSONString(body[i:end])
			if err != nil {
				return ModelSpan{}, ErrMalformedBody
			}
			if key != "model" {
				i = end
				continue
			}
			if seen {
				return ModelSpan{}, ErrDuplicateModel
			}
			seen = true
			v := skipSpace(body, colon+1)
			if v >= len(body) || body[v] != '"' {
				return ModelSpan{}, ErrMissingModel
			}
			vend, err := scanStringEnd(body, v)
			if err != nil {
				return ModelSpan{}, ErrMalformedBody
			}
			val, err := decodeJSONString(body[v:vend])
			if err != nil {
				return ModelSpan{}, ErrMalformedBody
			}
			found = ModelSpan{Value: val, Start: v, End: vend}
			i = vend
		default:
			// Scalar value: skip to the next structural byte.
			for i < len(body) && !isStructural(body[i]) {
				i++
			}
		}
	}
	if !seen {
		return ModelSpan{}, ErrMissingModel
	}
	return found, nil
}

// scanStringEnd returns the offset just past the closing quote of the
// JSON string starting at open (which must be a quote).
func scanStringEnd(body []byte, open int) (int, error) {
	for i := open + 1; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, ErrMalformedBody
}

// decodeJSONString decodes a quoted JSON string, including its surrounding
// quotes.
func decodeJSONString(raw []byte) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func skipSpace(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func isStructural(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}
