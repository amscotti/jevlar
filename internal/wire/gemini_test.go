package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type multipartTestPart struct {
	name        string
	filename    string
	contentType string
	body        []byte
}

func buildTestMultipart(t *testing.T, parts ...multipartTestPart) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := make(textproto.MIMEHeader)
		disp := fmt.Sprintf(`form-data; name="%s"`, p.name)
		if p.filename != "" {
			disp += fmt.Sprintf(`; filename="%s"`, p.filename)
		}
		if p.name != "" {
			h.Set("Content-Disposition", disp)
		} else {
			h.Set("Content-Disposition", "form-data")
		}
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart(%q): %v", p.name, err)
		}
		if _, err := pw.Write(p.body); err != nil {
			t.Fatalf("Write(%q): %v", p.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart Close: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func mustUnmarshalObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", raw, err)
	}
	return out
}

func TestTranslateGeminiRequest_JSONAndQuestions(t *testing.T) {
	t.Parallel()

	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}
	gifBytes := []byte("GIF89a\x01\x00\x01\x00")
	webpBytes := []byte("RIFF\x18\x00\x00\x00WEBPVP8 ")
	rawUnknownBytes := []byte{0x01, 0x02, 0x03, 0x04}

	pngB64 := base64.StdEncoding.EncodeToString(pngBytes)
	jpegB64 := base64.StdEncoding.EncodeToString(jpegBytes)
	gifB64 := base64.StdEncoding.EncodeToString(gifBytes)
	webpB64 := base64.StdEncoding.EncodeToString(webpBytes)
	unknownB64 := base64.StdEncoding.EncodeToString(rawUnknownBytes)

	cases := []struct {
		name        string
		body        string
		contentType string
		targetModel string
		want        string
	}{
		{
			name:        "text state and empty contentType",
			body:        `{"model":"jev-latest","state":"customer cannot connect stripe"}`,
			contentType: "",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":"customer cannot connect stripe"}`,
		},
		{
			name:        "unparseable contentType defaults to JSON",
			body:        `{"model":"jev-latest","state":"ok","assets":null}`,
			contentType: ";;;bad;;;",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":"ok"}`,
		},
		{
			name:        "application/json with charset and empty assets object",
			body:        `{"model":"jev-latest","state":"","assets":{}}`,
			contentType: "Application/JSON; charset=utf-8",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":""}`,
		},
		{
			name:        "object state compacted to string",
			body:        "{\n  \"model\": \"alias\",\n  \"state\": {\n    \"user\": \"alice\",\n    \"items\": [1, 2]\n  }\n}",
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":"{\"user\":\"alice\",\"items\":[1,2]}"}`,
		},
		{
			name:        "array state compacted to string",
			body:        `{"model":"alias","state":[1, "two", {"k": true}]}`,
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":"[1,\"two\",{\"k\":true}]"}`,
		},
		{
			name:        "preserves unknown top-level fields and extra_body",
			body:        `{"model":"alias","state":"hello","__unknown__":1,"extra_body":{"trace":true},"temperature":0.2}`,
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":"hello","__unknown__":1,"extra_body":{"trace":true},"temperature":0.2}`,
		},
		{
			name: "noul choice score box and non-object questions",
			body: `{
				"model": "alias",
				"state": "invoice issue",
				"questions": {
					"urgent": {
						"type": "noul",
						"instructions": {"rule": "check urgency", "items": [1, 2]},
						"criteria": {
							"true": {"label": "urgent"},
							"false": "not urgent",
							"unknown_extra": {"keep": true}
						}
					},
					"noul_nulls_and_existing": {
						"type": "noul",
						"instructions": null,
						"criteria": {
							"true": "ignored_old",
							"true_description": ["existing", "array"],
							"false": null
						}
					},
					"noul_non_object_criteria": {
						"type": "noul",
						"criteria": ["unexpected"]
					},
					"noul_no_true_false": {
						"type": "noul",
						"criteria": {"true_description": "already set"}
					},
					"team": {
						"type": "choice",
						"instructions": "pick team",
						"criteria": {
							"billing": "Payments",
							"tech": {"desc": "Bugs", "codes": [500, 502]},
							"none": null,
							"num": 42
						}
					},
					"choice_non_object_criteria": {
						"type": "choice",
						"criteria": null
					},
					"severity": {
						"type": "score",
						"instructions": ["rate", 1, 5],
						"criteria": ["Mild", {"level": "High", "weight": 2}, null, 10]
					},
					"score_non_array_criteria": {
						"type": "score",
						"criteria": {"bad": true}
					},
					"region": {
						"type": "box",
						"instructions": {"target": "red rectangle"},
						"criteria": {"min_area": 0.1}
					},
					"untouched_null_q": null,
					"untouched_str_q": "raw"
				}
			}`,
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want: `{
				"model": "gemini-decision-1",
				"input": "invoice issue",
				"questions": {
					"urgent": {
						"type": "noul",
						"instructions": "{\"rule\":\"check urgency\",\"items\":[1,2]}",
						"criteria": {
							"true_description": "{\"label\":\"urgent\"}",
							"false_description": "not urgent",
							"unknown_extra": {"keep": true}
						}
					},
					"noul_nulls_and_existing": {
						"type": "noul",
						"instructions": null,
						"criteria": {
							"true": "ignored_old",
							"true_description": "[\"existing\",\"array\"]",
							"false_description": null
						}
					},
					"noul_non_object_criteria": {
						"type": "noul",
						"criteria": ["unexpected"]
					},
					"noul_no_true_false": {
						"type": "noul",
						"criteria": {"true_description": "already set"}
					},
					"team": {
						"type": "choice",
						"instructions": "pick team",
						"criteria": {
							"billing": "Payments",
							"tech": "{\"desc\":\"Bugs\",\"codes\":[500,502]}",
							"none": null,
							"num": "42"
						}
					},
					"choice_non_object_criteria": {
						"type": "choice",
						"criteria": null
					},
					"severity": {
						"type": "score",
						"instructions": "[\"rate\",1,5]",
						"criteria": ["Mild", "{\"level\":\"High\",\"weight\":2}", null, "10"]
					},
					"score_non_array_criteria": {
						"type": "score",
						"criteria": {"bad": true}
					},
					"region": {
						"type": "box",
						"instructions": "{\"target\":\"red rectangle\"}",
						"criteria": {"min_area": 0.1}
					},
					"untouched_null_q": null,
					"untouched_str_q": "raw"
				}
			}`,
		},
		{
			name:        "non-object questions left untouched",
			body:        `{"model":"alias","state":"ok","questions":["not","an","object"]}`,
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","input":"ok","questions":["not","an","object"]}`,
		},
		{
			name: "inline data_base64 formats and deterministic sorted alias order",
			body: fmt.Sprintf(`{
				"model": "alias",
				"state": "compare images",
				"assets": {
					"z_unknown_sniff": {"type": "image", "data_base64": "  %s \n"},
					"d_webp": {"type": "image", "format": "webp", "data_base64": "%s"},
					"c_gif": {"type": "image", "format": "GIF", "data_base64": "%s"},
					"b_jpeg": {"type": "image", "format": "jpg", "data_base64": "%s"},
					"a_png_sniffed": {"type": "image", "data_base64": "%s"},
					"e_png_fmt": {"type": "image", "format": "png", "data_base64": "%s"}
				}
			}`, unknownB64, webpB64, gifB64, jpegB64, pngB64, pngB64),
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want: fmt.Sprintf(`{
				"model": "gemini-decision-1",
				"input": [
					{"type": "text", "text": "compare images"},
					{"type": "image", "data": "%s", "mime_type": "image/png"},
					{"type": "image", "data": "%s", "mime_type": "image/jpeg"},
					{"type": "image", "data": "%s", "mime_type": "image/gif"},
					{"type": "image", "data": "%s", "mime_type": "image/webp"},
					{"type": "image", "data": "%s", "mime_type": "image/png"},
					{"type": "image", "data": "%s", "mime_type": "application/octet-stream"}
				]
			}`, pngB64, jpegB64, gifB64, webpB64, pngB64, unknownB64),
		},
		{
			name: "empty state with inline base64 omits text part",
			body: fmt.Sprintf(`{
				"model": "alias",
				"state": "",
				"assets": {
					"photo": {"type": "image", "format": "jpeg", "data_base64": "%s"}
				}
			}`, jpegB64),
			contentType: "application/json",
			targetModel: "gemini-decision-1",
			want: fmt.Sprintf(`{
				"model": "gemini-decision-1",
				"input": [
					{"type": "image", "data": "%s", "mime_type": "image/jpeg"}
				]
			}`, jpegB64),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotBytes, err := TranslateGeminiRequest([]byte(tc.body), tc.contentType, tc.targetModel)
			if err != nil {
				t.Fatalf("TranslateGeminiRequest error: %v", err)
			}
			got := mustUnmarshalObject(t, gotBytes)
			want := mustUnmarshalObject(t, []byte(tc.want))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("mismatch:\n got: %s\nwant: %s", gotBytes, tc.want)
			}
		})
	}
}

func TestTranslateGeminiRequest_Multipart(t *testing.T) {
	t.Parallel()

	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4}
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 5, 6, 7}
	gifBytes := []byte("GIF87a1234")
	webpBytes := []byte("RIFF\x10\x00\x00\x00WEBP1234")

	reqJSON := `{
		"model": "jev-preview",
		"state": "inspect photos",
		"assets": {
			"second": {"type": "image", "part": "asset-1"},
			"first": {"type": "image", "part": "asset-0"},
			"third_gif": {"type": "image", "part": "asset-2"},
			"fourth_webp": {"type": "image", "part": "asset-3"}
		},
		"questions": {
			"q": {"type": "noul", "criteria": {"true": "yes", "false": "no"}}
		}
	}`

	body, ct := buildTestMultipart(t,
		multipartTestPart{name: "", body: []byte("ignored-unnamed")},
		multipartTestPart{name: "request", contentType: "application/json", body: []byte(reqJSON)},
		multipartTestPart{name: "asset-0", filename: "a.png", contentType: "IMAGE/PNG; charset=binary", body: pngBytes},
		multipartTestPart{name: "asset-1", filename: "b.jpg", contentType: "application/octet-stream", body: jpegBytes},
		multipartTestPart{name: "asset-2", filename: "c.gif", contentType: "", body: gifBytes},
		multipartTestPart{name: "asset-3", filename: "d.webp", contentType: "image/*", body: webpBytes},
	)

	gotBytes, err := TranslateGeminiRequest(body, ct, "gemini-decision-1")
	if err != nil {
		t.Fatalf("TranslateGeminiRequest multipart error: %v", err)
	}

	wantJSON := fmt.Sprintf(`{
		"model": "gemini-decision-1",
		"input": [
			{"type": "text", "text": "inspect photos"},
			{"type": "image", "data": "%s", "mime_type": "image/png"},
			{"type": "image", "data": "%s", "mime_type": "image/webp"},
			{"type": "image", "data": "%s", "mime_type": "image/jpeg"},
			{"type": "image", "data": "%s", "mime_type": "image/gif"}
		],
		"questions": {
			"q": {"type": "noul", "criteria": {"true_description": "yes", "false_description": "no"}}
		}
	}`,
		base64.StdEncoding.EncodeToString(pngBytes),
		base64.StdEncoding.EncodeToString(webpBytes),
		base64.StdEncoding.EncodeToString(jpegBytes),
		base64.StdEncoding.EncodeToString(gifBytes),
	)

	got := mustUnmarshalObject(t, gotBytes)
	want := mustUnmarshalObject(t, []byte(wantJSON))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multipart mismatch:\n got: %s\nwant: %s", gotBytes, wantJSON)
	}
}

func TestTranslateGeminiRequest_Errors(t *testing.T) {
	t.Parallel()

	missingReqBody, missingReqCT := buildTestMultipart(t,
		multipartTestPart{name: "asset-0", contentType: "image/png", body: []byte{1, 2, 3}},
	)
	dupReqBody, dupReqCT := buildTestMultipart(t,
		multipartTestPart{name: "request", contentType: "application/json", body: []byte(`{"state":"a"}`)},
		multipartTestPart{name: "request", contentType: "application/json", body: []byte(`{"state":"b"}`)},
	)
	missingPartBody, missingPartCT := buildTestMultipart(t,
		multipartTestPart{name: "request", contentType: "application/json", body: []byte(`{"state":"a","assets":{"p":{"part":"missing"}}}`)},
	)

	cases := []struct {
		name        string
		body        []byte
		contentType string
		targetModel string
		wantSubstr  string
	}{
		{
			name:        "empty targetModel",
			body:        []byte(`{"state":"x"}`),
			contentType: "application/json",
			targetModel: "",
			wantSubstr:  "targetModel",
		},
		{
			name:        "whitespace targetModel",
			body:        []byte(`{"state":"x"}`),
			contentType: "application/json",
			targetModel: "   ",
			wantSubstr:  "targetModel",
		},
		{
			name:        "unsupported media type",
			body:        []byte(`{"state":"x"}`),
			contentType: "text/plain",
			targetModel: "gemini-1",
			wantSubstr:  "unsupported media type",
		},
		{
			name:        "multipart missing boundary",
			body:        []byte(`--x`),
			contentType: "multipart/form-data",
			targetModel: "gemini-1",
			wantSubstr:  "boundary",
		},
		{
			name:        "multipart malformed body",
			body:        []byte(`not a multipart body`),
			contentType: "multipart/form-data; boundary=abc",
			targetModel: "gemini-1",
			wantSubstr:  "multipart",
		},
		{
			name:        "multipart malformed headers",
			body:        []byte("--abc\r\nBadHeaderLine\r\n\r\nbody\r\n--abc--\r\n"),
			contentType: "multipart/form-data; boundary=abc",
			targetModel: "gemini-1",
			wantSubstr:  "read multipart body",
		},
		{
			name:        "multipart truncated part body",
			body:        []byte("--abc\r\nContent-Disposition: form-data; name=\"request\"\r\n\r\n{\"state\":\"x\""),
			contentType: "multipart/form-data; boundary=abc",
			targetModel: "gemini-1",
			wantSubstr:  "read multipart part",
		},
		{
			name:        "multipart missing request part",
			body:        missingReqBody,
			contentType: missingReqCT,
			targetModel: "gemini-1",
			wantSubstr:  `missing "request"`,
		},
		{
			name:        "multipart duplicate request part",
			body:        dupReqBody,
			contentType: dupReqCT,
			targetModel: "gemini-1",
			wantSubstr:  `duplicate "request"`,
		},
		{
			name:        "multipart asset references missing part",
			body:        missingPartBody,
			contentType: missingPartCT,
			targetModel: "gemini-1",
			wantSubstr:  `missing multipart part "missing"`,
		},
		{
			name:        "empty JSON body",
			body:        []byte(``),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "JSON object",
		},
		{
			name:        "JSON array body",
			body:        []byte(`[1,2,3]`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "JSON object",
		},
		{
			name:        "JSON null body",
			body:        []byte(`null`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "JSON object",
		},
		{
			name:        "invalid JSON syntax",
			body:        []byte(`{"state":`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "parse request JSON",
		},
		{
			name:        "missing state field",
			body:        []byte(`{"model":"a"}`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  `"state"`,
		},
		{
			name:        "null state field",
			body:        []byte(`{"model":"a","state":null}`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  `"state"`,
		},
		{
			name:        "assets non-object",
			body:        []byte(`{"state":"ok","assets":[]}`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  `"assets"`,
		},
		{
			name:        "asset invalid base64",
			body:        []byte(`{"state":"ok","assets":{"a":{"data_base64":"!!!not-base64!!!"}}}`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "invalid base64",
		},
		{
			name:        "asset neither part nor data_base64",
			body:        []byte(`{"state":"ok","assets":{"a":{"type":"image","data_base64":"   "}}}`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "must specify either",
		},
		{
			name:        "json request references multipart part",
			body:        []byte(`{"state":"ok","assets":{"a":{"part":"asset-0"}}}`),
			contentType: "application/json",
			targetModel: "gemini-1",
			wantSubstr:  "missing multipart part",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := TranslateGeminiRequest(tc.body, tc.contentType, tc.targetModel)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestNormalizeGeminiResponse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        string
		targetModel string
		want        string
		wantErr     bool
	}{
		{
			name:        "backfills missing model and usage input/output tokens",
			body:        `{"answers":{"urgent":{"type":"noul","noul":0.91}},"usage":{"total_input_tokens":120,"total_output_tokens":15,"total_tokens":135,"input_tokens_by_modality":[{"modality":"TEXT","token_count":120}]}}`,
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","answers":{"urgent":{"type":"noul","noul":0.91}},"usage":{"input_tokens":120,"output_tokens":15,"total_input_tokens":120,"total_output_tokens":15,"total_tokens":135,"input_tokens_by_modality":[{"modality":"TEXT","token_count":120}]}}`,
		},
		{
			name:        "backfills null and empty model",
			body:        `{"model":null,"answers":{"team":{"type":"choice","choice":"billing"}}}`,
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","answers":{"team":{"type":"choice","choice":"billing"}}}`,
		},
		{
			name:        "backfills empty string model",
			body:        `{"model":"","answers":{}}`,
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-decision-1","answers":{}}`,
		},
		{
			name:        "preserves non-empty model and existing input/output tokens",
			body:        `{"model":"gemini-upstream-v2","answers":{"a":{"type":"score","score":3}},"usage":{"input_tokens":50,"output_tokens":10,"total_input_tokens":99,"total_output_tokens":88},"extra":"kept"}`,
			targetModel: "gemini-decision-1",
			want:        `{"model":"gemini-upstream-v2","answers":{"a":{"type":"score","score":3}},"usage":{"input_tokens":50,"output_tokens":10,"total_input_tokens":99,"total_output_tokens":88},"extra":"kept"}`,
		},
		{
			name:        "preserves non-string model and handles zero token counts and non-object usage",
			body:        `{"model":123,"usage":{"total_input_tokens":0,"total_output_tokens":0}}`,
			targetModel: "gemini-decision-1",
			want:        `{"model":123,"usage":{"input_tokens":0,"output_tokens":0,"total_input_tokens":0,"total_output_tokens":0}}`,
		},
		{
			name:        "leaves non-object usage untouched",
			body:        `{"model":"m","usage":null}`,
			targetModel: "gemini-decision-1",
			want:        `{"model":"m","usage":null}`,
		},
		{
			name:        "empty body errors",
			body:        "   ",
			targetModel: "gemini-decision-1",
			wantErr:     true,
		},
		{
			name:        "non-object JSON errors",
			body:        `["not","an","object"]`,
			targetModel: "gemini-decision-1",
			wantErr:     true,
		},
		{
			name:        "null JSON errors",
			body:        `null`,
			targetModel: "gemini-decision-1",
			wantErr:     true,
		},
		{
			name:        "invalid JSON syntax errors",
			body:        `{"model":`,
			targetModel: "gemini-decision-1",
			wantErr:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotBytes, err := NormalizeGeminiResponse([]byte(tc.body), tc.targetModel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %s", gotBytes)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeGeminiResponse error: %v", err)
			}
			got := mustUnmarshalObject(t, gotBytes)
			want := mustUnmarshalObject(t, []byte(tc.want))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("mismatch:\n got: %s\nwant: %s", gotBytes, tc.want)
			}
		})
	}
}

func TestIsGeminiInvalidKeyError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "401 empty body",
			status: http.StatusUnauthorized,
			body:   "",
			want:   true,
		},
		{
			name:   "401 invalid json",
			status: http.StatusUnauthorized,
			body:   "unauthorized",
			want:   true,
		},
		{
			name:   "400 API_KEY_INVALID",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}`,
			want:   true,
		},
		{
			name:   "403 API_KEY_EXPIRED in second detail",
			status: http.StatusForbidden,
			body:   `{"error":{"code":403,"details":[{"reason":"OTHER"},{"reason":"API_KEY_EXPIRED"}]}}`,
			want:   true,
		},
		{
			name:   "400 normal invalid_request",
			status: http.StatusBadRequest,
			body:   `{"error":{"type":"invalid_request","message":"unknown field"}}`,
			want:   false,
		},
		{
			name:   "403 other reason",
			status: http.StatusForbidden,
			body:   `{"error":{"details":[{"reason":"PERMISSION_DENIED"}]}}`,
			want:   false,
		},
		{
			name:   "429 even with API_KEY_INVALID payload",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"details":[{"reason":"API_KEY_INVALID"}]}}`,
			want:   false,
		},
		{
			name:   "500 internal error",
			status: http.StatusInternalServerError,
			body:   `{"error":{"details":[{"reason":"API_KEY_INVALID"}]}}`,
			want:   false,
		},
		{
			name:   "400 empty body",
			status: http.StatusBadRequest,
			body:   "",
			want:   false,
		},
		{
			name:   "400 invalid JSON",
			status: http.StatusBadRequest,
			body:   "{bad",
			want:   false,
		},
		{
			name:   "400 null error field",
			status: http.StatusBadRequest,
			body:   `{"error":null}`,
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsGeminiInvalidKeyError(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("IsGeminiInvalidKeyError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func FuzzTranslateGeminiRequest(f *testing.F) {
	f.Add([]byte(`{"model":"jev-latest","state":"hello","questions":{"q":{"type":"noul","instructions":"urgent?","criteria":{"true":"y","false":"n"}}}}`), "application/json", "gemini-decision-1")
	f.Add([]byte(`{"state":{"a":1},"assets":{"img":{"type":"image","format":"png","data_base64":"iVBORw0KGgo="}},"questions":{"c":{"type":"choice","criteria":{"a":{"k":1},"b":null}},"s":{"type":"score","criteria":[{"hi":1},"lo",null]}}}`), "", "gemini-decision-1")
	f.Add([]byte(`{"state":[1,2,3],"__unknown__":true}`), "application/json; charset=utf-8", "gemini-decision-1")

	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	if rw, err := mw.CreateFormField("request"); err == nil {
		_, _ = rw.Write([]byte(`{"state":"photo","assets":{"p":{"type":"image","part":"asset-0"}}}`))
	}
	if aw, err := mw.CreateFormFile("asset-0", "photo.png"); err == nil {
		_, _ = aw.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 1, 2})
	}
	_ = mw.Close()
	f.Add(mpBuf.Bytes(), mw.FormDataContentType(), "gemini-decision-1")

	f.Fuzz(func(t *testing.T, body []byte, contentType, targetModel string) {
		out, err := TranslateGeminiRequest(body, contentType, targetModel)
		if err != nil {
			return
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("TranslateGeminiRequest produced invalid JSON: %v (%s)", err, out)
		}
		if _, hasState := doc["state"]; hasState {
			t.Fatalf("outbound request still contains state: %s", out)
		}
		if _, hasAssets := doc["assets"]; hasAssets {
			t.Fatalf("outbound request still contains assets: %s", out)
		}
		if _, hasInput := doc["input"]; !hasInput {
			t.Fatalf("outbound request missing input: %s", out)
		}
		var model string
		if err := json.Unmarshal(doc["model"], &model); err != nil || (utf8.ValidString(targetModel) && model != targetModel) {
			t.Fatalf("outbound model = %q (err=%v), want %q", model, err, targetModel)
		}
	})
}
