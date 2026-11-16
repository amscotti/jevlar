// Package proxy implements Jevlar's request path: admission, inbound
// authentication, bounded body reading, target resolution, the attempt loop
// with its deadline budget, forwarding on pooled provider transports, response
// classification, and bounded response buffering with commit-after-read.
//
// For "systemone" providers (the default), proxy performs pure byte
// passthrough per ADR 0003: only the single declared "model" span is spliced
// when a target remaps the alias, leaving all other bytes untouched. For
// "gemini-decisions" providers, each attempt adapts System One requests and
// Gemini Decisions responses via internal/wire while keeping the client's
// decoded request body immutable across attempts so mixed-wire failover works
// seamlessly.
//
// The package owns no mutable cross-request state of its own: the health
// registry, key pools, transports, and concurrency slots are borrowed from
// the runtime snapshot it loads once per request, and a response is written
// to the client only once it has been fully read.
package proxy
