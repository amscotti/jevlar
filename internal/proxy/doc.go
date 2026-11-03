// Package proxy owns the request path: admission, inbound authentication,
// bounded body reads, the single declared "model" splice, forwarding on
// pooled provider transports, response classification and bounded response
// buffering with commit-after-read. Bytes outside the spliced value are
// forwarded unchanged; a response is written to the client only once it has
// been fully read. The implementation lands with the pass-through proxy
// work; this package pins its contract only.
package proxy
