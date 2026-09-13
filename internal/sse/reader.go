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
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
)

// dataField is the only SSE field name carrying payload bytes.
const dataField = "data"

// Reader decodes an SSE response body into a channel of data payloads.
// It exits when the context is canceled or the body closes (stream loss).
type Reader struct {
	ctx  context.Context
	body io.ReadCloser
	out  chan<- []byte
}

// NewReader wraps an HTTP response body as an SSE stream.
func NewReader(ctx context.Context, resp *http.Response, out chan<- []byte) *Reader {
	return &Reader{ctx: ctx, body: resp.Body, out: out}
}

// Start begins decoding; it closes the channel when the stream ends. Per
// SSE framing, consecutive `data:` lines of one frame are joined by
// newlines before delivery; comment lines and other field names are
// ignored.
func (r *Reader) Start() {
	go r.run()
}

// run decodes frames until the body ends or the context is canceled. A
// frame is delivered only when a blank line terminates it; an incomplete
// trailing frame at stream end (truncation) is discarded.
func (r *Reader) run() {
	defer close(r.out)
	br := bufio.NewReader(r.body)
	var dataLines []string
	for {
		line, err := readLine(br)
		if err != nil {
			return
		}
		if line == "" {
			if len(dataLines) > 0 {
				payload := decodeOne(dataLines)
				dataLines = nil
				select {
				case r.out <- payload:
				case <-r.ctx.Done():
					return
				}
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if field == dataField {
			dataLines = append(dataLines, value)
		}
	}
}

// readLine reads one SSE line, accepting LF, CRLF, and bare CR
// terminators. At end of input it returns the partial line and the error.
func readLine(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		c, err := br.ReadByte()
		if err != nil {
			return b.String(), err
		}
		switch c {
		case '\n':
			return b.String(), nil
		case '\r':
			if next, perr := br.Peek(1); perr == nil && next[0] == '\n' {
				if _, rerr := br.ReadByte(); rerr != nil {
					return b.String(), rerr
				}
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
}

// decodeOne assembles one frame's payload from its data lines. It is
// exported for tests of the framing rules.
func decodeOne(dataLines []string) []byte {
	return []byte(strings.Join(dataLines, "\n"))
}
