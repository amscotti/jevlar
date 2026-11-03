package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxModelIDLen bounds a model id. Model ids are opaque tokens, not
// prose; the limit keeps a spliced value small and the audit path simple.
const maxModelIDLen = 200

// ErrInvalidModelID reports a model id that cannot be safely spliced into
// a JSON string without escaping. Configuration validation already
// restricts ids to this charset; the check here is the fail-closed
// backstop for a caller that skipped it.
var ErrInvalidModelID = fmt.Errorf("model id contains characters outside [A-Za-z0-9._:-]")

// ValidModelID reports whether id is safe to splice verbatim.
func ValidModelID(id string) bool {
	if id == "" || len(id) > maxModelIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// RewriteModel returns a new body with the value at span replaced by model.
//
// Only the bytes of the model value change: everything before span.Start
// and after span.End is copied verbatim, which is what makes the rewrite
// provable. The input is never mutated — attempts share one buffer.
func RewriteModel(body []byte, span ModelSpan, model string) ([]byte, error) {
	if !ValidModelID(model) {
		return nil, ErrInvalidModelID
	}
	if span.Start < 0 || span.End > len(body) || span.Start >= span.End {
		return nil, fmt.Errorf("model span %d:%d outside body of length %d", span.Start, span.End, len(body))
	}
	quoted, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("encode model id: %w", err)
	}
	out := make([]byte, 0, len(body)-(span.End-span.Start)+len(quoted))
	out = append(out, body[:span.Start]...)
	out = append(out, quoted...)
	out = append(out, body[span.End:]...)
	return out, nil
}

// describeModelIDs renders a list for an error message without allocating
// when it is short.
func describeModelIDs(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}
