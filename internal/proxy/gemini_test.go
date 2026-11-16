package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/testutil"
	"github.com/amscotti/jevlar/internal/usage"
	"github.com/klauspost/compress/zstd"
)

type geminiHarness struct {
	h        *Handler
	rt       *runtime.Runtime
	cfg      *config.Config
	reg      *health.Registry
	upstream map[string]*testutil.FakeUpstream
}

func newGeminiHarness(t *testing.T, buildCfg func(ups map[string]*testutil.FakeUpstream) *config.Config, names ...string) *geminiHarness {
	t.Helper()
	ups := make(map[string]*testutil.FakeUpstream, len(names))
	for _, name := range names {
		ups[name] = testutil.NewFakeUpstream(t)
	}
	cfg := buildCfg(ups)
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("runtime.Build: %v", err)
	}
	reg := health.New()
	t.Cleanup(testutil.WireWithHealth(rt, reg))
	for _, p := range rt.Providers {
		if p.Pool != nil {
			p.Pool.SetRand(func(int) int { return 0 })
		}
	}
	m := metrics.New(metrics.Options{Version: "test", GoVersion: "go-test"})
	h := New(func() *runtime.Runtime { return rt }, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	return &geminiHarness{h: h, rt: rt, cfg: cfg, reg: reg, upstream: ups}
}

func geminiBaseConfig(providers []config.Provider, alias string, targets []config.ModelTarget, maxAttempts int) *config.Config {
	return &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen:              "127.0.0.1:0",
			RequestTimeoutMs:    10_000,
			MaxBodyBytes:        65_536,
			MaxResponseBytes:    65_536,
			MaxInflight:         64,
			ShutdownGraceMs:     1000,
			ReadHeaderTimeoutMs: 5000,
			LogLevel:            "error",
		},
		Auth:      config.Auth{Keys: []config.AuthKey{{ID: "test", Hash: testClientHash(), RPM: 600}}},
		Providers: providers,
		Models: map[string]config.Model{
			alias: {
				Targets:     targets,
				Policy:      config.PolicyPriority,
				MaxAttempts: maxAttempts,
			},
		},
		Breaker: config.Breaker{Threshold: 5, CooldownMs: 30_000},
	}
}

func postSystemOne(h *Handler, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeSystemOne(rec, req)
	return rec
}

func decodeJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", raw, err)
	}
	return out
}

func TestGemini_JSONTranslationNormalizationAndUsage(t *testing.T) {
	t.Parallel()
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{{
				Name:           "gemini",
				BaseURL:        ups["gemini"].URL(),
				Path:           "/v1beta/decisions",
				Auth:           config.AuthXGoogAPIKey,
				WireFormat:     config.WireGeminiDecisions,
				Models:         []string{"gemini-decision-1"},
				TimeoutMs:      5000,
				MaxConcurrency: 8,
				APIKeys:        []config.APIKey{{ID: "gk1", Resolved: "goog-secret-1"}},
			}},
			"jev-latest",
			[]config.ModelTarget{{Provider: "gemini", Model: "gemini-decision-1"}},
			1,
		)
	}, "gemini")

	x.upstream["gemini"].SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {
				"urgent": {"type": "noul", "noul": 0.91},
				"team": {"type": "choice", "choice": "billing", "confidence": 0.85},
				"severity": {"type": "score", "score": 0.7}
			},
			"usage": {
				"total_input_tokens": 210,
				"total_output_tokens": 0,
				"total_tokens": 210
			}
		}`))
	})

	reqBody := []byte(`{
		"model": "jev-latest",
		"state": {"message": "Customer cannot pay invoice"},
		"questions": {
			"urgent": {
				"type": "noul",
				"instructions": "Is this urgent?",
				"criteria": {"true": "Blocked or time-sensitive", "false": "Routine"}
			},
			"team": {
				"type": "choice",
				"instructions": "Route to team",
				"criteria": {"billing": "Billing support", "tech": "Technical"}
			},
			"severity": {
				"type": "score",
				"instructions": "Severity level",
				"criteria": ["Low", "High"]
			}
		}
	}`)

	rec := postSystemOne(x.h, reqBody, map[string]string{
		"Content-Type":   "application/json",
		"X-Goog-Api-Key": "client-forged-goog-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	upBody, upHeader := x.upstream["gemini"].LastRequest()
	if got := upHeader.Get("X-Goog-Api-Key"); got != "goog-secret-1" {
		t.Fatalf("upstream X-Goog-Api-Key = %q, want goog-secret-1", got)
	}
	if got := upHeader.Get("Authorization"); got != "" {
		t.Fatalf("upstream Authorization = %q, want stripped", got)
	}
	if got := upHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json", got)
	}

	sent := decodeJSONMap(t, upBody)
	if sent["model"] != "gemini-decision-1" {
		t.Fatalf("upstream model = %v, want gemini-decision-1", sent["model"])
	}
	if _, hasState := sent["state"]; hasState {
		t.Fatalf("upstream request still contains state: %v", sent)
	}
	if gotInput, ok := sent["input"].(string); !ok || gotInput != `{"message":"Customer cannot pay invoice"}` {
		t.Fatalf("upstream input = %#v, want compacted JSON string", sent["input"])
	}
	questions := sent["questions"].(map[string]any)
	noulCrit := questions["urgent"].(map[string]any)["criteria"].(map[string]any)
	if noulCrit["true_description"] != "Blocked or time-sensitive" || noulCrit["false_description"] != "Routine" {
		t.Fatalf("noul criteria = %#v, want true_description/false_description", noulCrit)
	}
	if _, hasTrue := noulCrit["true"]; hasTrue {
		t.Fatalf("noul criteria still has true: %#v", noulCrit)
	}
	if _, hasFalse := noulCrit["false"]; hasFalse {
		t.Fatalf("noul criteria still has false: %#v", noulCrit)
	}

	resp := decodeJSONMap(t, rec.Body.Bytes())
	if resp["model"] != "gemini-decision-1" {
		t.Fatalf("normalized response model = %v, want gemini-decision-1", resp["model"])
	}
	respUsage := resp["usage"].(map[string]any)
	if respUsage["input_tokens"] != float64(210) || respUsage["output_tokens"] != float64(0) {
		t.Fatalf("normalized usage = %#v, want input_tokens=210, output_tokens=0", respUsage)
	}

	rows, misses := x.h.Tracker().Snapshot()
	if misses != 0 {
		t.Fatalf("usage misses = %d, want 0", misses)
	}
	wantKey := usage.Key{Client: "test", Provider: "gemini", APIKey: "gk1", Model: "gemini-decision-1"}
	counts, ok := rows[wantKey]
	if !ok {
		t.Fatalf("missing usage row for %+v (rows: %+v)", wantKey, rows)
	}
	if counts.Requests != 1 || counts.InputTokens != 210 || counts.OutputTokens != 0 {
		t.Fatalf("counts = %+v, want 1 req, 210 input, 0 output", counts)
	}

	decs := x.h.Tracker().DecisionsSnapshot()
	dc, ok := decs["gemini-decision-1"]
	if !ok {
		t.Fatalf("missing decision counts for gemini-decision-1: %+v", decs)
	}
	if dc.Total != 2 || dc.Choices["team"]["billing"] != 1 ||
		dc.Confidence["team"]["0.8-1.0"] != 1 || dc.Confidence["urgent"]["0.8-1.0"] != 1 {
		t.Fatalf("decision counts = %+v", dc)
	}
}

func TestGemini_ZstdMultipartImageUpload(t *testing.T) {
	t.Parallel()
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{{
				Name:           "gemini",
				BaseURL:        ups["gemini"].URL(),
				Path:           "/v1beta/decisions",
				Auth:           config.AuthXGoogAPIKey,
				WireFormat:     config.WireGeminiDecisions,
				Models:         []string{"gemini-decision-1"},
				TimeoutMs:      5000,
				MaxConcurrency: 8,
				APIKeys:        []config.APIKey{{ID: "gk1", Resolved: "goog-secret-1"}},
			}},
			"jev-latest",
			[]config.ModelTarget{{Provider: "gemini", Model: "gemini-decision-1"}},
			1,
		)
	}, "gemini")

	x.upstream["gemini"].SetBody([]byte(`{"answers":{"has_logo":{"type":"noul","noul":1.0}},"usage":{"total_input_tokens":500,"total_output_tokens":0}}`))

	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4}
	requestPart := `{"model":"jev-latest","state":"Inspect image","assets":{"img":{"type":"image","part":"asset-0"}},"questions":{"has_logo":{"type":"noul","instructions":"Logo visible?"}}}`
	mpBody := buildMultipart(t, "gem-boundary", requestPart, map[string][]byte{"asset-0": pngBytes})

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(mpBody, nil)

	rec := postSystemOne(x.h, compressed, map[string]string{
		"Content-Type":     "multipart/form-data; boundary=gem-boundary",
		"Content-Encoding": "zstd",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	upBody, upHeader := x.upstream["gemini"].LastRequest()
	if got := upHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json", got)
	}
	if got := upHeader.Get("Content-Encoding"); got != "" {
		t.Fatalf("upstream Content-Encoding = %q, want empty", got)
	}
	if got := upHeader.Get("X-Goog-Api-Key"); got != "goog-secret-1" {
		t.Fatalf("upstream X-Goog-Api-Key = %q, want goog-secret-1", got)
	}

	sent := decodeJSONMap(t, upBody)
	parts, ok := sent["input"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("upstream input = %#v, want 2-element array", sent["input"])
	}
	textPart := parts[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "Inspect image" {
		t.Fatalf("text part = %#v", textPart)
	}
	imgPart := parts[1].(map[string]any)
	wantB64 := base64.StdEncoding.EncodeToString(pngBytes)
	if imgPart["type"] != "image" || imgPart["mime_type"] != "image/png" || imgPart["data"] != wantB64 {
		t.Fatalf("image part = %#v, want png %q", imgPart, wantB64)
	}
}

func TestGemini_KeyRotationOnAPIKeyInvalid(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		seenKeys []string
	)
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{{
				Name:           "gemini",
				BaseURL:        ups["gemini"].URL(),
				Path:           "/v1beta/decisions",
				Auth:           config.AuthXGoogAPIKey,
				WireFormat:     config.WireGeminiDecisions,
				Models:         []string{"gemini-decision-1"},
				TimeoutMs:      5000,
				MaxConcurrency: 8,
				APIKeys: []config.APIKey{
					{ID: "k1", Resolved: "bad-goog-key"},
					{ID: "k2", Resolved: "good-goog-key"},
				},
			}},
			"jev-latest",
			[]config.ModelTarget{{Provider: "gemini", Model: "gemini-decision-1"}},
			2,
		)
	}, "gemini")

	x.upstream["gemini"].SetHandler(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Goog-Api-Key")
		mu.Lock()
		seenKeys = append(seenKeys, key)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if key == "bad-goog-key" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"answers":{"ok":{"type":"noul","noul":1.0}},"usage":{"total_input_tokens":10,"total_output_tokens":0}}`))
	})

	reqBody := []byte(`{"model":"jev-latest","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`)
	rec := postSystemOne(x.h, reqBody, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after key rotation (body: %s)", rec.Code, rec.Body.String())
	}

	mu.Lock()
	gotKeys := append([]string(nil), seenKeys...)
	mu.Unlock()
	if len(gotKeys) != 2 || gotKeys[0] != "bad-goog-key" || gotKeys[1] != "good-goog-key" {
		t.Fatalf("keys tried = %v, want [bad-goog-key good-goog-key]", gotKeys)
	}
	if st := x.reg.Key(health.KeyRef{Provider: "gemini", ID: "k1"}).State(); st != health.StateDisabled {
		t.Fatalf("k1 state = %s, want disabled", st)
	}
	if st := x.reg.Key(health.KeyRef{Provider: "gemini", ID: "k2"}).State(); st != health.StateHealthy {
		t.Fatalf("k2 state = %s, want healthy", st)
	}
	if x.reg.Breaker("gemini").Open() {
		t.Fatal("breaker opened on invalid key; auth error must not trip provider breaker")
	}
}

func TestGemini_MixedWireFailover_GeminiToSystemOne(t *testing.T) {
	t.Parallel()
	for _, failStatus := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(failStatus), func(t *testing.T) {
			t.Parallel()
			x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
				return geminiBaseConfig(
					[]config.Provider{
						{
							Name:           "gemini",
							BaseURL:        ups["gemini"].URL(),
							Path:           "/v1beta/decisions",
							Auth:           config.AuthXGoogAPIKey,
							WireFormat:     config.WireGeminiDecisions,
							Models:         []string{"gemini-decision-1"},
							TimeoutMs:      5000,
							MaxConcurrency: 8,
							APIKeys:        []config.APIKey{{ID: "gk", Resolved: "goog-secret"}},
						},
						{
							Name:           "typesafe",
							BaseURL:        ups["typesafe"].URL(),
							Path:           "/v1/systemone",
							Auth:           config.AuthBearer,
							WireFormat:     config.WireSystemOne,
							Models:         []string{"jev-latest"},
							TimeoutMs:      5000,
							MaxConcurrency: 8,
							APIKeys:        []config.APIKey{{ID: "tk", Resolved: "ts-secret"}},
						},
					},
					"jev-latest",
					[]config.ModelTarget{
						{Provider: "gemini", Model: "gemini-decision-1"},
						{Provider: "typesafe", Model: "jev-latest"},
					},
					2,
				)
			}, "gemini", "typesafe")

			x.upstream["gemini"].SetStatus(failStatus)
			x.upstream["typesafe"].SetBody([]byte(`{"model":"jev-latest","answers":{"ok":{"type":"noul","noul":0.8}},"usage":{"input_tokens":42,"output_tokens":0}}`))

			plain := []byte(`{"model":"jev-latest","state":"keep me byte-identical","questions":{"ok":{"type":"noul","instructions":"ok?","criteria":{"true":"yes","false":"no"}}}}`)
			enc, err := zstd.NewWriter(nil)
			if err != nil {
				t.Fatalf("zstd.NewWriter: %v", err)
			}
			compressed := enc.EncodeAll(plain, nil)
			wantSHA := sha256.Sum256(compressed)

			rec := postSystemOne(x.h, compressed, map[string]string{
				"Content-Type":     "application/json",
				"Content-Encoding": "zstd",
				"X-Goog-Api-Key":   "client-key-must-be-stripped",
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 via systemone fallback (body: %s)", rec.Code, rec.Body.String())
			}

			gemBody, gemHdr := x.upstream["gemini"].LastRequest()
			if gemHdr.Get("X-Goog-Api-Key") != "goog-secret" || gemHdr.Get("Authorization") != "" {
				t.Fatalf("gemini headers wrong: X-Goog-Api-Key=%q Authorization=%q", gemHdr.Get("X-Goog-Api-Key"), gemHdr.Get("Authorization"))
			}
			if gemHdr.Get("Content-Encoding") != "" || gemHdr.Get("Content-Type") != "application/json" {
				t.Fatalf("gemini content headers wrong: Content-Encoding=%q Content-Type=%q", gemHdr.Get("Content-Encoding"), gemHdr.Get("Content-Type"))
			}
			gemDoc := decodeJSONMap(t, gemBody)
			if gemDoc["model"] != "gemini-decision-1" || gemDoc["input"] != "keep me byte-identical" {
				t.Fatalf("gemini translated doc = %#v", gemDoc)
			}

			tsBody, tsHdr := x.upstream["typesafe"].LastRequest()
			if gotSHA := sha256.Sum256(tsBody); gotSHA != wantSHA {
				t.Fatalf("systemone body sha256 = %s, want untouched %s", hex.EncodeToString(gotSHA[:]), hex.EncodeToString(wantSHA[:]))
			}
			if tsHdr.Get("Content-Encoding") != "zstd" {
				t.Fatalf("systemone Content-Encoding = %q, want zstd", tsHdr.Get("Content-Encoding"))
			}
			if tsHdr.Get("Authorization") != "Bearer ts-secret" {
				t.Fatalf("systemone Authorization = %q, want Bearer ts-secret", tsHdr.Get("Authorization"))
			}
			if tsHdr.Get("X-Goog-Api-Key") != "" {
				t.Fatalf("systemone X-Goog-Api-Key = %q, want stripped", tsHdr.Get("X-Goog-Api-Key"))
			}
		})
	}
}

func TestGemini_MixedWireFailover_SystemOneToGemini(t *testing.T) {
	t.Parallel()
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{
				{
					Name:           "typesafe",
					BaseURL:        ups["typesafe"].URL(),
					Path:           "/v1/systemone",
					Auth:           config.AuthBearer,
					WireFormat:     config.WireSystemOne,
					Models:         []string{"jev-latest"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
					APIKeys:        []config.APIKey{{ID: "tk", Resolved: "ts-secret"}},
				},
				{
					Name:           "gemini",
					BaseURL:        ups["gemini"].URL(),
					Path:           "/v1beta/decisions",
					Auth:           config.AuthXGoogAPIKey,
					WireFormat:     config.WireGeminiDecisions,
					Models:         []string{"gemini-decision-1"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
					APIKeys:        []config.APIKey{{ID: "gk", Resolved: "goog-secret"}},
				},
			},
			"jev-latest",
			[]config.ModelTarget{
				{Provider: "typesafe", Model: "jev-latest"},
				{Provider: "gemini", Model: "gemini-decision-1"},
			},
			2,
		)
	}, "typesafe", "gemini")

	x.upstream["typesafe"].SetStatus(http.StatusServiceUnavailable)
	x.upstream["gemini"].SetBody([]byte(`{"answers":{"urgent":{"type":"noul","noul":0.25}},"usage":{"total_input_tokens":55,"total_output_tokens":0}}`))

	reqBody := []byte(`{"model":"jev-latest","state":"billing question","questions":{"urgent":{"type":"noul","instructions":"urgent?","criteria":{"true":"yes","false":"no"}}}}`)
	rec := postSystemOne(x.h, reqBody, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover to gemini (body: %s)", rec.Code, rec.Body.String())
	}

	tsBody, tsHdr := x.upstream["typesafe"].LastRequest()
	if !bytes.Equal(tsBody, reqBody) {
		t.Fatalf("systemone body mutated: got %s, want %s", tsBody, reqBody)
	}
	if tsHdr.Get("Authorization") != "Bearer ts-secret" {
		t.Fatalf("systemone Authorization = %q, want Bearer ts-secret", tsHdr.Get("Authorization"))
	}

	gemBody, gemHdr := x.upstream["gemini"].LastRequest()
	if gemHdr.Get("X-Goog-Api-Key") != "goog-secret" || gemHdr.Get("Authorization") != "" {
		t.Fatalf("gemini headers wrong: X-Goog-Api-Key=%q Authorization=%q", gemHdr.Get("X-Goog-Api-Key"), gemHdr.Get("Authorization"))
	}
	gemDoc := decodeJSONMap(t, gemBody)
	if gemDoc["model"] != "gemini-decision-1" || gemDoc["input"] != "billing question" {
		t.Fatalf("gemini translated doc = %#v", gemDoc)
	}
	crit := gemDoc["questions"].(map[string]any)["urgent"].(map[string]any)["criteria"].(map[string]any)
	if crit["true_description"] != "yes" || crit["false_description"] != "no" {
		t.Fatalf("gemini noul criteria = %#v", crit)
	}

	resp := decodeJSONMap(t, rec.Body.Bytes())
	if resp["model"] != "gemini-decision-1" {
		t.Fatalf("response model = %v, want gemini-decision-1", resp["model"])
	}
}

func TestGemini_Client400PassthroughNoDisableNoFailover(t *testing.T) {
	t.Parallel()
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{
				{
					Name:           "gemini",
					BaseURL:        ups["gemini"].URL(),
					Path:           "/v1beta/decisions",
					Auth:           config.AuthXGoogAPIKey,
					WireFormat:     config.WireGeminiDecisions,
					Models:         []string{"gemini-decision-1"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
					APIKeys:        []config.APIKey{{ID: "gk1", Resolved: "goog-secret"}},
				},
				{
					Name:           "fallback",
					BaseURL:        ups["fallback"].URL(),
					Path:           "/v1/systemone",
					Auth:           config.AuthNone,
					WireFormat:     config.WireSystemOne,
					Models:         []string{"jev-latest"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
				},
			},
			"jev-latest",
			[]config.ModelTarget{
				{Provider: "gemini", Model: "gemini-decision-1"},
				{Provider: "fallback", Model: "jev-latest"},
			},
			2,
		)
	}, "gemini", "fallback")

	const upstreamErr = `{"error":{"code":400,"message":"Unsupported question type: box","status":"INVALID_ARGUMENT"}}`
	x.upstream["gemini"].SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamErr))
	})

	reqBody := []byte(`{"model":"jev-latest","state":"photo","questions":{"loc":{"type":"box","instructions":"locate"}}}`)
	rec := postSystemOne(x.h, reqBody, map[string]string{"Content-Type": "application/json"})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamErr {
		t.Fatalf("body = %q, want upstream 400 body %q", rec.Body.String(), upstreamErr)
	}
	if x.upstream["fallback"].Requests() != 0 {
		t.Fatalf("fallback was contacted %d times on client 400", x.upstream["fallback"].Requests())
	}
	if st := x.reg.Key(health.KeyRef{Provider: "gemini", ID: "gk1"}).State(); st != health.StateHealthy {
		t.Fatalf("gk1 state = %s, want healthy on non-auth 400", st)
	}
}

func TestGemini_InvalidTranslationReturns400WithoutFailover(t *testing.T) {
	t.Parallel()
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{
				{
					Name:           "gemini",
					BaseURL:        ups["gemini"].URL(),
					Path:           "/v1beta/decisions",
					Auth:           config.AuthXGoogAPIKey,
					WireFormat:     config.WireGeminiDecisions,
					Models:         []string{"gemini-decision-1"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
					APIKeys:        []config.APIKey{{ID: "gk1", Resolved: "goog-secret"}},
				},
				{
					Name:           "fallback",
					BaseURL:        ups["fallback"].URL(),
					Path:           "/v1/systemone",
					Auth:           config.AuthNone,
					WireFormat:     config.WireSystemOne,
					Models:         []string{"jev-latest"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
				},
			},
			"jev-latest",
			[]config.ModelTarget{
				{Provider: "gemini", Model: "gemini-decision-1"},
				{Provider: "fallback", Model: "jev-latest"},
			},
			2,
		)
	}, "gemini", "fallback")

	requestPart := `{"model":"jev-latest","state":"photo","assets":{"img":{"type":"image","part":"missing-part"}}}`
	mpBody := buildMultipart(t, "bad-part", requestPart, map[string][]byte{"asset-0": {0x89, 'P', 'N', 'G'}})

	rec := postSystemOne(x.h, mpBody, map[string]string{
		"Content-Type": "multipart/form-data; boundary=bad-part",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), TypeInvalidRequest) {
		t.Fatalf("body = %s, want invalid_request", rec.Body.String())
	}
	if x.upstream["gemini"].Requests() != 0 || x.upstream["fallback"].Requests() != 0 {
		t.Fatalf("upstreams contacted on translation error: gemini=%d fallback=%d",
			x.upstream["gemini"].Requests(), x.upstream["fallback"].Requests())
	}
}

func TestGemini_Corrupt2xxResponseIsRetryable502(t *testing.T) {
	t.Parallel()
	t.Run("single target returns 502", func(t *testing.T) {
		t.Parallel()
		x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
			return geminiBaseConfig(
				[]config.Provider{{
					Name:           "gemini",
					BaseURL:        ups["gemini"].URL(),
					Path:           "/v1beta/decisions",
					Auth:           config.AuthXGoogAPIKey,
					WireFormat:     config.WireGeminiDecisions,
					Models:         []string{"gemini-decision-1"},
					TimeoutMs:      5000,
					MaxConcurrency: 8,
					APIKeys:        []config.APIKey{{ID: "gk1", Resolved: "goog-secret"}},
				}},
				"jev-latest",
				[]config.ModelTarget{{Provider: "gemini", Model: "gemini-decision-1"}},
				1,
			)
		}, "gemini")

		x.upstream["gemini"].SetBody([]byte(`{corrupt-json`))
		reqBody := []byte(`{"model":"jev-latest","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`)
		rec := postSystemOne(x.h, reqBody, map[string]string{"Content-Type": "application/json"})

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "could not be normalized") {
			t.Fatalf("body = %s, want normalization error message", rec.Body.String())
		}
	})

	t.Run("fails over to next target", func(t *testing.T) {
		t.Parallel()
		x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
			return geminiBaseConfig(
				[]config.Provider{
					{
						Name:           "gemini",
						BaseURL:        ups["gemini"].URL(),
						Path:           "/v1beta/decisions",
						Auth:           config.AuthXGoogAPIKey,
						WireFormat:     config.WireGeminiDecisions,
						Models:         []string{"gemini-decision-1"},
						TimeoutMs:      5000,
						MaxConcurrency: 8,
						APIKeys:        []config.APIKey{{ID: "gk1", Resolved: "goog-secret"}},
					},
					{
						Name:           "typesafe",
						BaseURL:        ups["typesafe"].URL(),
						Path:           "/v1/systemone",
						Auth:           config.AuthBearer,
						WireFormat:     config.WireSystemOne,
						Models:         []string{"jev-latest"},
						TimeoutMs:      5000,
						MaxConcurrency: 8,
						APIKeys:        []config.APIKey{{ID: "tk1", Resolved: "ts-secret"}},
					},
				},
				"jev-latest",
				[]config.ModelTarget{
					{Provider: "gemini", Model: "gemini-decision-1"},
					{Provider: "typesafe", Model: "jev-latest"},
				},
				2,
			)
		}, "gemini", "typesafe")

		x.upstream["gemini"].SetBody([]byte(`not-json`))
		x.upstream["typesafe"].SetBody([]byte(`{"model":"jev-latest","answers":{"ok":{"type":"noul","noul":1.0}},"usage":{"input_tokens":10,"output_tokens":0}}`))

		reqBody := []byte(`{"model":"jev-latest","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`)
		rec := postSystemOne(x.h, reqBody, map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 after failover from corrupt 2xx (body: %s)", rec.Code, rec.Body.String())
		}
		if x.upstream["typesafe"].Requests() != 1 {
			t.Fatalf("typesafe requests = %d, want 1", x.upstream["typesafe"].Requests())
		}
	})
}

func TestUpstream_StripsInboundXGoogAPIKeyOnAuthNone(t *testing.T) {
	t.Parallel()
	x := newGeminiHarness(t, func(ups map[string]*testutil.FakeUpstream) *config.Config {
		return geminiBaseConfig(
			[]config.Provider{{
				Name:           "up",
				BaseURL:        ups["up"].URL(),
				Path:           "/v1/systemone",
				Auth:           config.AuthNone,
				WireFormat:     config.WireSystemOne,
				Models:         []string{"jev-latest"},
				TimeoutMs:      5000,
				MaxConcurrency: 8,
			}},
			"jev-latest",
			[]config.ModelTarget{{Provider: "up", Model: "jev-latest"}},
			1,
		)
	}, "up")

	rec := postSystemOne(x.h, []byte(validBody), map[string]string{
		"Content-Type":   "application/json",
		"X-Goog-Api-Key": "client-forged-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	_, hdr := x.upstream["up"].LastRequest()
	if got := hdr.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want stripped", got)
	}
	if got := hdr.Get("X-Goog-Api-Key"); got != "" {
		t.Fatalf("X-Goog-Api-Key = %q, want stripped", got)
	}
}
