package sse

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDecodeOne(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{name: "single line", in: []string{`{"type":"message"}`}, want: `{"type":"message"}`},
		{name: "multi line joined by newline", in: []string{"first", "second"}, want: "first\nsecond"},
		{name: "empty lines preserved inside payload", in: []string{"a", "", "b"}, want: "a\n\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(decodeOne(tt.in)); got != tt.want {
				t.Errorf("decodeOne(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// stream delivers the payloads received from one Reader over body.
func stream(t *testing.T, body string) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan []byte, 8)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	NewReader(ctx, resp, out).Start()
	var got []string
	for payload := range out {
		got = append(got, string(payload))
	}
	return got
}

func TestReaderFrames(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "crush style data only frames",
			body: "data: {\"type\":\"message\"}\n\ndata: {\"type\":\"session\"}\n\n",
			want: []string{`{"type":"message"}`, `{"type":"session"}`},
		},
		{
			name: "crlf terminators",
			body: "data: one\r\n\r\ndata: two\r\n\r\n",
			want: []string{"one", "two"},
		},
		{
			name: "bare cr terminators",
			body: "data: one\r\rdata: two\r\r",
			want: []string{"one", "two"},
		},
		{
			name: "multi line data joined",
			body: "data: a\ndata: b\n\n",
			want: []string{"a\nb"},
		},
		{
			name: "comments and other fields ignored",
			body: ": keepalive\nevent: chat\ndata: x\nid: 7\nretry: 100\n\ndata: y\n\n",
			want: []string{"x", "y"},
		},
		{
			name: "no space after colon",
			body: "data:x\n\n",
			want: []string{"x"},
		},
		{
			name: "truncated final frame discarded",
			body: "data: good\n\ndata: trunc",
			want: []string{"good"},
		},
		{
			name: "frame without blank line at eof discarded",
			body: "data: a\ndata: b\n",
			want: nil,
		},
		{
			name: "empty stream",
			body: "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stream(t, tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("frame %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReaderContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan []byte)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: a\n\n"))}
	NewReader(ctx, resp, out).Start()
	if got := <-out; string(got) != "a" {
		t.Fatalf("first payload = %q", got)
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("channel not closed after context cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after context cancel")
	}
}
