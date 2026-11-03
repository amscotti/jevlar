package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildMultipart assembles a multipart body the way the official SDK does:
// a JSON "request" part, then one binary part per asset.
func buildMultipart(t *testing.T, boundary, requestJSON string, parts map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writePart := func(name, contentType string, body []byte) {
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString(`Content-Disposition: form-data; name="` + name + `"`)
		if contentType != "application/json" {
			buf.WriteString(`; filename="` + name + `"`)
		}
		buf.WriteString("\r\nContent-Type: " + contentType + "\r\n\r\n")
		buf.Write(body)
		buf.WriteString("\r\n")
	}
	writePart("request", "application/json", []byte(requestJSON))
	for name, data := range parts {
		writePart(name, "image/png", data)
	}
	buf.WriteString("--" + boundary + "--\r\n")
	return buf.Bytes()
}

// TestFindModelInMultipart_LocatesTheRequestPart pins the multipart scan
// and that the returned span is an offset into the whole body.
func TestFindModelInMultipart_LocatesTheRequestPart(t *testing.T) {
	t.Parallel()
	request := `{"model":"jev-preview","state":"photo","questions":{"red":{"type":"box"}}}`
	body := buildMultipart(t, "b0undary", request, map[string][]byte{"asset-0": {1, 2, 3, 4}})

	span, err := FindModelInMultipart(body, "b0undary")
	if err != nil {
		t.Fatalf("FindModelInMultipart: %v", err)
	}
	if span.Value != "jev-preview" {
		t.Fatalf("Value = %q", span.Value)
	}
	if got := string(body[span.Start:span.End]); got != `"jev-preview"` {
		t.Fatalf("span covers %q, want the quoted model value", got)
	}
}

// TestFindModelInMultipart_RewritePreservesEveryOtherByte pins the
// integrity property for uploads: splicing the model inside the JSON part
// leaves the boundaries, headers and image bytes untouched.
func TestFindModelInMultipart_RewritePreservesEveryOtherByte(t *testing.T) {
	t.Parallel()
	request := `{"model":"jev-latest","state":"photo","questions":{"red":{"type":"box"}}}`
	image := bytes.Repeat([]byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0xFF}, 500)
	body := buildMultipart(t, "xyz", request, map[string][]byte{"asset-0": image})

	span, err := FindModelInMultipart(body, "xyz")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	out, err := RewriteModel(body, span, "jev-preview")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Contains(out, []byte(`"model":"jev-preview"`)) {
		t.Fatalf("model not rewritten: %q", out[:120])
	}
	// The binary part is byte-identical and still framed correctly.
	if !bytes.Contains(out, image) {
		t.Fatal("image bytes changed or were dropped")
	}
	if !bytes.HasPrefix(out, []byte("--xyz\r\n")) || !bytes.HasSuffix(out, []byte("--xyz--\r\n")) {
		t.Fatal("multipart framing damaged")
	}
	// Rewriting back restores the original exactly.
	back, err := FindModelInMultipart(out, "xyz")
	if err != nil {
		t.Fatalf("re-find: %v", err)
	}
	restored, err := RewriteModel(out, back, "jev-latest")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !bytes.Equal(restored, body) {
		t.Fatal("restoring the model did not reproduce the original upload")
	}
}

// TestFindModelInMultipart_RejectsAmbiguousOrMalformedBodies pins the
// fail-closed cases.
func TestFindModelInMultipart_RejectsAmbiguousOrMalformedBodies(t *testing.T) {
	t.Parallel()
	good := `{"model":"m"}`

	cases := []struct {
		name string
		body []byte
		want error
	}{
		{
			name: "missing request part",
			body: []byte("--b\r\nContent-Disposition: form-data; name=\"asset-0\"\r\n" +
				"Content-Type: image/png\r\n\r\n\x89PNG\r\n--b--\r\n"),
			want: ErrMissingRequestPart,
		},
		{
			name: "empty body",
			body: nil,
			want: ErrBadMultipart,
		},
		{
			name: "no boundary",
			body: []byte("not multipart at all"),
			want: ErrBadMultipart,
		},
		{
			name: "request part is not a JSON object",
			body: buildMultipart(t, "b", "not json", nil),
			want: ErrMalformedBody,
		},
		{
			name: "request part has no model",
			body: buildMultipart(t, "b", `{"state":"x"}`, nil),
			want: ErrMissingModel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := FindModelInMultipart(tc.body, "b")
			if err == nil {
				t.Fatal("accepted an unroutable body")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// Two request parts are ambiguous: Jevlar must not pick one.
	dup := []byte("--b\r\nContent-Disposition: form-data; name=\"request\"\r\n\r\n" + good +
		"\r\n--b\r\nContent-Disposition: form-data; name=\"request\"\r\n\r\n" + good + "\r\n--b--\r\n")
	if _, err := FindModelInMultipart(dup, "b"); err == nil {
		t.Fatal("accepted a body with two request parts")
	}
}

// TestFindModelByMediaType_RoutesByContentType pins the dispatcher.
func TestFindModelByMediaType_RoutesByContentType(t *testing.T) {
	t.Parallel()
	request := `{"model":"jev-preview"}`
	mp := buildMultipart(t, "b", request, nil)

	if span, err := FindModelByMediaType(mp, "multipart/form-data; boundary=b"); err != nil || span.Value != "jev-preview" {
		t.Fatalf("multipart: %v %q", err, span.Value)
	}
	if span, err := FindModelByMediaType([]byte(request), "application/json; charset=utf-8"); err != nil || span.Value != "jev-preview" {
		t.Fatalf("json: %v %q", err, span.Value)
	}
	// No content type falls back to JSON, which is what most clients send.
	if span, err := FindModelByMediaType([]byte(request), ""); err != nil || span.Value != "jev-preview" {
		t.Fatalf("no content type: %v %q", err, span.Value)
	}
	if _, err := FindModelByMediaType(mp, "multipart/form-data"); err == nil {
		t.Fatal("multipart without a boundary was accepted")
	}
	if _, err := FindModelByMediaType([]byte("x"), "application/octet-stream"); err == nil {
		t.Fatal("an unsupported content type was accepted")
	}
}

// TestDecodeRequestBody_HandlesIdentityAndZstd pins the decoder, including
// the decompression-bomb cap.
func TestDecodeRequestBody_HandlesIdentityAndZstd(t *testing.T) {
	t.Parallel()
	plain := []byte(`{"model":"jev-latest","state":"` + strings.Repeat("x", 4000) + `"}`)

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := enc.EncodeAll(plain, nil)
	if len(compressed) >= len(plain) {
		t.Fatalf("test payload did not compress: %d vs %d", len(compressed), len(plain))
	}

	if got, err := decodeRequestBody(plain, "", int64(len(plain))); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("identity: %v", err)
	}
	if got, err := decodeRequestBody(plain, "IDENTITY", int64(len(plain))); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("explicit identity: %v", err)
	}
	got, err := decodeRequestBody(compressed, "zstd", int64(len(plain)))
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("zstd round trip changed the body")
	}

	// The cap applies to the decoded size, not the compressed size.
	if _, err := decodeRequestBody(compressed, "zstd", 64); !errors.Is(err, ErrDecodedTooLarge) {
		t.Fatalf("oversized decoded body: %v", err)
	}
	if _, err := decodeRequestBody(plain, "br", int64(len(plain))); err == nil {
		t.Fatal("unsupported encoding accepted")
	}
	if _, err := decodeRequestBody([]byte("not zstd"), "zstd", 1024); err == nil {
		t.Fatal("garbage accepted as zstd")
	}

	// A compressed body must still expose its model.
	span, err := FindModelByMediaType(mustDecode(t, compressed), "application/json")
	if err != nil || span.Value != "jev-latest" {
		t.Fatalf("model after decode: %v %q", err, span.Value)
	}
}

func mustDecode(t *testing.T, compressed []byte) []byte {
	t.Helper()
	out, err := decodeRequestBody(compressed, "zstd", 1<<20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestMultipartUpload_IsForwardedAndRewritten pins the end-to-end handler
// path for an SDK-style upload: the upstream receives the multipart body
// with the target's model and the image bytes intact.
func TestMultipartUpload_IsForwardedAndRewritten(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{{provider: "a", model: "nimble"}}, "priority", 1)

	request := `{"model":"jev-latest","state":"photo","questions":{"red":{"type":"box","instructions":"find it"}}}`
	image := bytes.Repeat([]byte{0x89, 0x50, 0x4E, 0x47}, 200)
	body := buildMultipart(t, "sdk-boundary", request, map[string][]byte{"asset-0": image})

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=sdk-boundary")
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	rec := httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got, _ := x.upstream["a"].LastRequest()
	if !bytes.Contains(got, []byte(`"model":"nimble"`)) {
		t.Fatalf("upstream body did not carry the target model")
	}
	if !bytes.Contains(got, image) {
		t.Fatal("image bytes did not survive the gateway")
	}
	if !bytes.HasPrefix(got, []byte("--sdk-boundary\r\n")) {
		t.Fatal("multipart framing changed")
	}
}

// TestCompressedUpload_IsForwardedCompressedWhenNoRewriteIsNeeded pins that
// Jevlar preserves the client's compression on the common path.
func TestCompressedUpload_IsForwardedCompressedWhenNoRewriteIsNeeded(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	plain := []byte(`{"model":"jev-latest","state":"` + strings.Repeat("y", 4000) + `"}`)
	enc, _ := zstd.NewWriter(nil)
	compressed := enc.EncodeAll(plain, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	rec := httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got, header := x.upstream.LastRequest()
	if !bytes.Equal(got, compressed) {
		t.Fatal("compressed body was not forwarded byte-for-byte")
	}
	if header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("Content-Encoding = %q, want zstd preserved", header.Get("Content-Encoding"))
	}
}

// TestCompressedUpload_IsDecodedWhenRewritten pins the rewrite path: the
// body is sent decoded because the compressed bytes no longer match.
func TestCompressedUpload_IsDecodedWhenRewritten(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{{provider: "a", model: "nimble"}}, "priority", 1)
	plain := []byte(`{"model":"jev-latest","state":"` + strings.Repeat("z", 4000) + `"}`)
	enc, _ := zstd.NewWriter(nil)
	compressed := enc.EncodeAll(plain, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	rec := httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got, header := x.upstream["a"].LastRequest()
	if header.Get("Content-Encoding") != "" {
		t.Fatalf("rewritten body still advertises %q", header.Get("Content-Encoding"))
	}
	var decoded struct {
		Model string `json:"model"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	if decoded.Model != "nimble" || len(decoded.State) != 4000 {
		t.Fatalf("rewritten body = model %q, state len %d", decoded.Model, len(decoded.State))
	}
}
