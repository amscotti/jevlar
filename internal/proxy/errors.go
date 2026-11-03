package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Error types in Jevlar's client-facing contract. Upstream 4xx bodies pass
// through untouched; only errors Jevlar generates use this envelope.
const (
	// TypeUnauthorized means the presented client key is missing or unknown.
	TypeUnauthorized = "unauthorized"
	// TypeQuotaExceeded means the client's own rpm/tpm budget is spent.
	TypeQuotaExceeded = "quota_exceeded"
	// TypeCapacityExhausted means Jevlar had no usable upstream capacity
	// (every key cooling or disabled, or every provider saturated). It is
	// not the client's fault and is retried after the Retry-After value.
	TypeCapacityExhausted = "capacity_exhausted"
	// TypeInvalidRequest means the request itself is wrong.
	TypeInvalidRequest = "invalid_request"
	// TypeNotFound means the named model has no configured target.
	TypeNotFound = "not_found"
	// TypeUnavailable means every target failed for an upstream reason.
	TypeUnavailable = "unavailable"
	// TypeInternal means Jevlar failed in a way it could not classify.
	TypeInternal = "internal"
)

// errorEnvelope is the documented Jevlar-generated error body.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// writeError emits a Jevlar-generated error with the request id attached.
// retryAfterMS, when positive, is emitted as Retry-After seconds (rounded
// up, minimum 1) so clients can back off without guessing.
func writeError(w http.ResponseWriter, status int, typ, message, requestID string, retryAfterMS int64) {
	if retryAfterMS > 0 {
		secs := (retryAfterMS + 999) / 1000
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorBody{Type: typ, Message: message, RequestID: requestID},
	})
}
