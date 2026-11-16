package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
)

const (
	mediaTypeJSON        = "application/json"
	mediaTypeMultipart   = "multipart/form-data"
	multipartRequestPart = "request"
)

type multipartPart struct {
	data        []byte
	contentType string
}

type assetEntry struct {
	Type       string `json:"type"`
	Format     string `json:"format"`
	DataBase64 string `json:"data_base64"`
	Part       string `json:"part"`
}

type geminiTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type geminiImagePart struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mime_type"`
}

type googleRPCErrorEnvelope struct {
	Error *struct {
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// TranslateGeminiRequest converts a System One JSON or multipart/form-data
// request body into a Gemini Decisions API JSON request targeting targetModel.
func TranslateGeminiRequest(body []byte, contentType, targetModel string) ([]byte, error) {
	if strings.TrimSpace(targetModel) == "" {
		return nil, errors.New("targetModel must be non-empty")
	}

	requestJSON, parts, err := parseRequestPayload(body, contentType)
	if err != nil {
		return nil, err
	}

	doc, ok, err := unmarshalObject(requestJSON)
	if err != nil {
		return nil, fmt.Errorf("parse request JSON: %w", err)
	}
	if !ok {
		return nil, errors.New("request body must be a JSON object")
	}

	rawState, hasState := doc["state"]
	if !hasState || isJSONNull(rawState) {
		return nil, errors.New(`request body missing required "state" field`)
	}

	rawAssets, hasAssets := doc["assets"]
	inputRaw, err := buildGeminiInput(rawState, rawAssets, hasAssets, parts)
	if err != nil {
		return nil, err
	}

	modelRaw, err := marshalRaw(targetModel)
	if err != nil {
		return nil, fmt.Errorf("encode targetModel: %w", err)
	}

	delete(doc, "state")
	delete(doc, "assets")
	doc["model"] = modelRaw
	doc["input"] = inputRaw

	if rawQuestions, ok := doc["questions"]; ok {
		transformed, err := transformQuestions(rawQuestions)
		if err != nil {
			return nil, fmt.Errorf("transform questions: %w", err)
		}
		doc["questions"] = transformed
	}

	return marshalRaw(doc)
}

// NormalizeGeminiResponse ensures a Gemini Decisions JSON response carries a
// non-empty "model" and mirrors "total_input_tokens"/"total_output_tokens" into
// "input_tokens"/"output_tokens" when absent, preserving all other fields.
func NormalizeGeminiResponse(body []byte, targetModel string) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("response body is empty")
	}

	doc, ok, err := unmarshalObject(body)
	if err != nil {
		return nil, fmt.Errorf("parse response JSON: %w", err)
	}
	if !ok {
		return nil, errors.New("response body must be a JSON object")
	}

	if needsModelBackfill(doc) {
		modelRaw, err := marshalRaw(targetModel)
		if err != nil {
			return nil, fmt.Errorf("encode targetModel: %w", err)
		}
		doc["model"] = modelRaw
	}

	if rawUsage, ok := doc["usage"]; ok {
		normalizedUsage, err := normalizeUsage(rawUsage)
		if err != nil {
			return nil, fmt.Errorf("normalize usage: %w", err)
		}
		doc["usage"] = normalizedUsage
	}

	return marshalRaw(doc)
}

// IsGeminiInvalidKeyError reports whether an upstream HTTP status and body
// indicate an invalid or expired Gemini API key.
func IsGeminiInvalidKeyError(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return false
	}
	if !isJSONObject(body) {
		return false
	}

	var env googleRPCErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return false
	}
	for _, detail := range env.Error.Details {
		if detail.Reason == "API_KEY_INVALID" || detail.Reason == "API_KEY_EXPIRED" {
			return true
		}
	}
	return false
}

func parseRequestPayload(body []byte, contentType string) ([]byte, map[string]multipartPart, error) {
	mediaType, params, ok := parseMediaType(contentType)
	if !ok {
		// Missing, empty, or unparseable Content-Type defaults to JSON.
		return body, nil, nil
	}

	switch mediaType {
	case mediaTypeJSON:
		return body, nil, nil
	case mediaTypeMultipart:
		boundary := params["boundary"]
		if boundary == "" {
			return nil, nil, errors.New("multipart/form-data Content-Type missing boundary")
		}
		return readMultipartPayload(body, boundary)
	default:
		return nil, nil, fmt.Errorf("unsupported media type %q", mediaType)
	}
}

func parseMediaType(contentType string) (string, map[string]string, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", nil, false
	}
	return mediaType, params, true
}

func readMultipartPayload(body []byte, boundary string) ([]byte, map[string]multipartPart, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var requestJSON []byte
	hasRequest := false
	parts := make(map[string]multipartPart)

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read multipart body: %w", err)
		}

		data, err := readAndClosePart(part)
		if err != nil {
			return nil, nil, fmt.Errorf("read multipart part: %w", err)
		}

		name := part.FormName()
		if name == multipartRequestPart {
			if hasRequest {
				return nil, nil, fmt.Errorf("multipart body contains duplicate %q part", multipartRequestPart)
			}
			requestJSON = data
			hasRequest = true
			continue
		}
		if name == "" {
			continue
		}

		parts[name] = multipartPart{
			data:        data,
			contentType: normalizePartContentType(part.Header.Get("Content-Type"), data),
		}
	}

	if !hasRequest {
		return nil, nil, fmt.Errorf("multipart body missing %q part", multipartRequestPart)
	}
	return requestJSON, parts, nil
}

func readAndClosePart(part *multipart.Part) ([]byte, error) {
	data, readErr := io.ReadAll(part)
	closeErr := part.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func normalizePartContentType(headerCT string, data []byte) string {
	if headerCT != "" {
		if mt, _, err := mime.ParseMediaType(headerCT); err == nil && strings.HasPrefix(mt, "image/") && mt != "image/*" {
			return mt
		}
	}
	return sniffImageMIME(data)
}

func sniffImageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return http.DetectContentType(data)
	}
}

func buildGeminiInput(rawState, rawAssets json.RawMessage, hasAssets bool, parts map[string]multipartPart) (json.RawMessage, error) {
	stateText, err := stateToText(rawState)
	if err != nil {
		return nil, fmt.Errorf(`parse "state": %w`, err)
	}

	if !hasAssets || isJSONNull(rawAssets) {
		return marshalRaw(stateText)
	}

	var assets map[string]assetEntry
	if err := json.Unmarshal(rawAssets, &assets); err != nil {
		return nil, fmt.Errorf(`parse "assets": %w`, err)
	}
	if len(assets) == 0 {
		return marshalRaw(stateText)
	}

	aliases := make([]string, 0, len(assets))
	for alias := range assets {
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)

	contentParts := make([]any, 0, len(aliases)+1)
	if stateText != "" {
		contentParts = append(contentParts, geminiTextPart{
			Type: "text",
			Text: stateText,
		})
	}
	for _, alias := range aliases {
		imgPart, err := resolveAsset(alias, assets[alias], parts)
		if err != nil {
			return nil, err
		}
		contentParts = append(contentParts, imgPart)
	}

	return marshalRaw(contentParts)
}

func resolveAsset(alias string, entry assetEntry, parts map[string]multipartPart) (geminiImagePart, error) {
	if entry.Part != "" {
		part, ok := parts[entry.Part]
		if !ok {
			return geminiImagePart{}, fmt.Errorf("asset %q references missing multipart part %q", alias, entry.Part)
		}
		return geminiImagePart{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(part.data),
			MimeType: part.contentType,
		}, nil
	}

	if trimmed := strings.TrimSpace(entry.DataBase64); trimmed != "" {
		decoded, err := base64.StdEncoding.DecodeString(trimmed)
		if err != nil {
			return geminiImagePart{}, fmt.Errorf("asset %q has invalid base64: %w", alias, err)
		}
		return geminiImagePart{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(decoded),
			MimeType: mimeFromFormatOrBytes(entry.Format, decoded),
		}, nil
	}

	return geminiImagePart{}, fmt.Errorf("asset %q must specify either %q or %q", alias, "part", "data_base64")
}

func mimeFromFormatOrBytes(format string, data []byte) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png", "image/png":
		return "image/png"
	case "jpg", "jpeg", "image/jpeg":
		return "image/jpeg"
	case "webp", "image/webp":
		return "image/webp"
	case "gif", "image/gif":
		return "image/gif"
	default:
		return sniffImageMIME(data)
	}
}

func stateToText(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if isJSONString(trimmed) {
		var s string
		err := json.Unmarshal(trimmed, &s)
		return s, err
	}
	var buf bytes.Buffer
	err := json.Compact(&buf, trimmed)
	return buf.String(), err
}

func transformQuestions(raw json.RawMessage) (json.RawMessage, error) {
	questions, ok, err := unmarshalObject(raw)
	if err != nil || !ok {
		return raw, err
	}

	for name, rawQ := range questions {
		transformed, err := transformQuestion(rawQ)
		if err != nil {
			return nil, fmt.Errorf("question %q: %w", name, err)
		}
		questions[name] = transformed
	}

	return marshalRaw(questions)
}

func transformQuestion(raw json.RawMessage) (json.RawMessage, error) {
	qMap, ok, err := unmarshalObject(raw)
	if err != nil || !ok {
		return raw, err
	}

	if err := compactMapFields(qMap, "instructions"); err != nil {
		return nil, err
	}

	var qType string
	if rawType, ok := qMap["type"]; ok && isJSONString(rawType) {
		if err := json.Unmarshal(rawType, &qType); err != nil {
			return nil, err
		}
	}

	if rawCriteria, ok := qMap["criteria"]; ok {
		transformed, err := transformCriteria(qType, rawCriteria)
		if err != nil {
			return nil, err
		}
		qMap["criteria"] = transformed
	}

	return marshalRaw(qMap)
}

func transformCriteria(qType string, raw json.RawMessage) (json.RawMessage, error) {
	switch qType {
	case "noul":
		return transformNoulCriteria(raw)
	case "choice":
		return transformChoiceCriteria(raw)
	case "score":
		return transformScoreCriteria(raw)
	default:
		return raw, nil
	}
}

func transformNoulCriteria(raw json.RawMessage) (json.RawMessage, error) {
	criteria, ok, err := unmarshalObject(raw)
	if err != nil || !ok {
		return raw, err
	}

	renameCriteriaKey(criteria, "true", "true_description")
	renameCriteriaKey(criteria, "false", "false_description")

	if err := compactMapFields(criteria, "true_description", "false_description"); err != nil {
		return nil, err
	}

	return marshalRaw(criteria)
}

func renameCriteriaKey(criteria map[string]json.RawMessage, from, to string) {
	val, hasFrom := criteria[from]
	if !hasFrom {
		return
	}
	if _, hasTo := criteria[to]; hasTo {
		return
	}
	criteria[to] = val
	delete(criteria, from)
}

func transformChoiceCriteria(raw json.RawMessage) (json.RawMessage, error) {
	criteria, ok, err := unmarshalObject(raw)
	if err != nil || !ok {
		return raw, err
	}

	if err := compactMapFields(criteria); err != nil {
		return nil, err
	}

	return marshalRaw(criteria)
}

func transformScoreCriteria(raw json.RawMessage) (json.RawMessage, error) {
	if !isJSONArray(raw) {
		return raw, nil
	}

	var criteria []json.RawMessage
	if err := json.Unmarshal(raw, &criteria); err != nil {
		return nil, err
	}

	for i, val := range criteria {
		compacted, changed, err := compactFieldToJSONString(val)
		if err != nil {
			return nil, err
		}
		if changed {
			criteria[i] = compacted
		}
	}

	return marshalRaw(criteria)
}

func compactMapFields(m map[string]json.RawMessage, keys ...string) error {
	if len(keys) == 0 {
		for k, val := range m {
			compacted, changed, err := compactFieldToJSONString(val)
			if err != nil {
				return err
			}
			if changed {
				m[k] = compacted
			}
		}
		return nil
	}

	for _, k := range keys {
		val, ok := m[k]
		if !ok {
			continue
		}
		compacted, changed, err := compactFieldToJSONString(val)
		if err != nil {
			return err
		}
		if changed {
			m[k] = compacted
		}
	}
	return nil
}

func compactFieldToJSONString(raw json.RawMessage) (json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONNull(trimmed) || isJSONString(trimmed) {
		return nil, false, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, false, err
	}
	encoded, err := marshalRaw(buf.String())
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func needsModelBackfill(doc map[string]json.RawMessage) bool {
	rawModel, ok := doc["model"]
	if !ok || isJSONNull(rawModel) {
		return true
	}
	if !isJSONString(rawModel) {
		return false
	}
	var model string
	if err := json.Unmarshal(rawModel, &model); err != nil {
		return false
	}
	return model == ""
}

func normalizeUsage(raw json.RawMessage) (json.RawMessage, error) {
	usageMap, ok, err := unmarshalObject(raw)
	if err != nil || !ok {
		return raw, err
	}

	if _, hasIn := usageMap["input_tokens"]; !hasIn {
		if totalIn, ok := usageMap["total_input_tokens"]; ok && !isJSONNull(totalIn) {
			usageMap["input_tokens"] = totalIn
		}
	}
	if _, hasOut := usageMap["output_tokens"]; !hasOut {
		if totalOut, ok := usageMap["total_output_tokens"]; ok && !isJSONNull(totalOut) {
			usageMap["output_tokens"] = totalOut
		}
	}

	return marshalRaw(usageMap)
}

func unmarshalObject(raw []byte) (map[string]json.RawMessage, bool, error) {
	if !isJSONObject(raw) {
		return nil, false, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, err
	}
	return m, true, nil
}

func marshalRaw(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func isJSONArray(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func isJSONString(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '"'
}

func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
