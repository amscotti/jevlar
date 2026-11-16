// Package wire isolates upstream wire-protocol adaptations from internal/proxy,
// keeping the default System One path pure byte passthrough.
//
// Today it implements Google's Gemini Decisions API ("gemini-decisions")
// request translation, response normalization, and invalid-key error
// classification using only the Go standard library.
package wire
