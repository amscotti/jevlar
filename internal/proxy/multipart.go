package proxy

import (
	"bytes"
	"fmt"
	"mime"
	"strings"
)

// Multipart media type constants. The provider's SDK sends a System One
// request as multipart/form-data as soon as it carries images: a JSON part
// named "request" followed by one binary part per image. Jevlar must find
// the model inside that JSON part while forwarding every byte unchanged.
const (
	mediaTypeMultipart = "multipart/form-data"
	mediaTypeJSON      = "application/json"
	// multipartRequestPart is the name of the JSON part.
	multipartRequestPart = "request"
)

// ErrUnsupportedMediaType reports a request body Jevlar cannot route on.
var ErrUnsupportedMediaType = fmt.Errorf("unsupported Content-Type; use application/json or multipart/form-data")

// ErrBadMultipart reports a malformed or ambiguous multipart body.
var ErrBadMultipart = fmt.Errorf("request body is not a valid multipart/form-data upload")

// ErrMissingRequestPart reports a multipart body without the JSON part the
// model is read from.
var ErrMissingRequestPart = fmt.Errorf("multipart body has no %q part", multipartRequestPart)

// FindModelByMediaType locates the top-level model for any body Jevlar
// accepts: a plain JSON document, or a multipart upload whose "request"
// part is that document.
//
// The returned span is an offset into body, so the same RewriteModel splice
// works for both encodings.
func FindModelByMediaType(body []byte, contentType string) (ModelSpan, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A missing or unparseable Content-Type is treated as JSON: that is
		// what the overwhelming majority of clients send, and a body that is
		// not JSON fails the scan anyway.
		return FindModel(body)
	}
	switch mediaType {
	case mediaTypeJSON:
		return FindModel(body)
	case mediaTypeMultipart:
		boundary := params["boundary"]
		if boundary == "" {
			return ModelSpan{}, fmt.Errorf("%w: missing boundary", ErrBadMultipart)
		}
		return FindModelInMultipart(body, boundary)
	default:
		return ModelSpan{}, ErrUnsupportedMediaType
	}
}

// FindModelInMultipart returns the model span inside the "request" part of
// a multipart body, with offsets relative to the whole body.
//
// Parsing is deliberately minimal: Jevlar only needs the JSON part, and
// every byte — boundary, part headers, binary parts — is forwarded exactly
// as received.
func FindModelInMultipart(body []byte, boundary string) (ModelSpan, error) {
	if boundary == "" || len(body) == 0 {
		return ModelSpan{}, ErrBadMultipart
	}
	delimiter := []byte("--" + boundary)
	closing := []byte("--" + boundary + "--")
	crlf := []byte("\r\n")

	pos := 0
	var requestSpan *ModelSpan
	found := 0
	sawPart := false

	for pos < len(body) {
		// Find the next delimiter line. It is either right where the
		// previous part ended (pos points at it) or preceded by CRLF.
		var delimAt int
		if bytes.HasPrefix(body[pos:], delimiter) {
			delimAt = pos
		} else {
			rel := bytes.Index(body[pos:], append(append([]byte{}, crlf...), delimiter...))
			if rel < 0 {
				break
			}
			delimAt = pos + rel + len(crlf)
		}
		if bytes.HasPrefix(body[delimAt:], closing) {
			break
		}
		contentStart := delimAt + len(delimiter)
		// Skip transport padding and the CRLF that ends the delimiter line.
		if i := bytes.Index(body[contentStart:], crlf); i >= 0 {
			contentStart += i + 2
		} else {
			return ModelSpan{}, ErrBadMultipart
		}

		headerEnd := bytes.Index(body[contentStart:], []byte("\r\n\r\n"))
		if headerEnd < 0 {
			return ModelSpan{}, ErrBadMultipart
		}
		headerBlock := body[contentStart : contentStart+headerEnd]
		partBodyStart := contentStart + headerEnd + 4

		// The part body ends at CRLF + the next delimiter.
		next := bytes.Index(body[partBodyStart:], append(append([]byte{}, crlf...), delimiter...))
		if next < 0 {
			return ModelSpan{}, ErrBadMultipart
		}
		partBodyEnd := partBodyStart + next

		sawPart = true
		if partName(headerBlock) == multipartRequestPart {
			found++
			if found > 1 {
				return ModelSpan{}, fmt.Errorf("%w: more than one %q part", ErrBadMultipart, multipartRequestPart)
			}
			inner, err := FindModel(body[partBodyStart:partBodyEnd])
			if err != nil {
				return ModelSpan{}, err
			}
			span := ModelSpan{
				Value: inner.Value,
				Start: inner.Start + partBodyStart,
				End:   inner.End + partBodyStart,
			}
			requestSpan = &span
		}

		// Continue after this part's delimiter.
		pos = partBodyEnd + len(crlf)
	}

	if !sawPart {
		return ModelSpan{}, ErrBadMultipart
	}
	if requestSpan == nil {
		return ModelSpan{}, ErrMissingRequestPart
	}
	return *requestSpan, nil
}

// partName extracts the form-data name from a part's header block.
func partName(headerBlock []byte) string {
	for _, line := range strings.Split(string(headerBlock), "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Disposition") {
			continue
		}
		disposition, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || disposition != "form-data" {
			return ""
		}
		return params["name"]
	}
	return ""
}
