// Package sse implements the small SSE reader used by crushapi
// (DESIGN §10.1: stdlib net/http plus a hand-rolled frame decoder).
//
// Crush's workspace event stream (GET /v1/workspaces/{id}/events) writes
// only `data: {json}` frames: there are no named `event:` or `id:` lines
// (verified against Crush v0.94.1, internal/server/proto.go). Event
// discrimination happens on the JSON envelope's type field inside the
// data payload, handled by crushapi.
package sse

import (
	"context"
	"net/http"
)

const notImplemented = "not implemented"

// Reader decodes an SSE response body into a channel of data payloads.
// It exits when the context is canceled or the body closes (stream loss).
type Reader struct{}

// NewReader wraps an HTTP response body as an SSE stream.
func NewReader(ctx context.Context, resp *http.Response, out chan<- []byte) *Reader {
	panic(notImplemented)
}

// Start begins decoding; it closes the channel when the stream ends. Per
// SSE framing, consecutive `data:` lines of one frame are joined by
// newlines before delivery; comment lines and other field names are
// ignored.
func (r *Reader) Start() { panic(notImplemented) }

// decodeOne assembles one frame's payload from its data lines. It is
// exported for tests of the framing rules.
func decodeOne(dataLines []string) []byte { panic(notImplemented) }
