package render

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text unchanged", in: "hello world", want: "hello world"},
		{name: "permitted layout controls kept", in: "line1\nline2\tcol\rend", want: "line1\nline2\tcol\rend"},
		{name: "ANSI SGR color stripped", in: "\x1b[31mred\x1b[0m text", want: "red text"},
		{name: "CSI with params and cursor final", in: "a\x1b[1;2Hb", want: "ab"},
		{name: "embedded escape aborts CSI", in: "\x1b[1;\x1b[0mHx", want: "Hx"},
		{name: "OSC title terminated by BEL stripped", in: "\x1b]0;evil title\x07body", want: "body"},
		{name: "OSC terminated by ST stripped", in: "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", want: "link"},
		{name: "charset escape stripped", in: "\x1b(BA", want: "A"},
		{name: "lone escape at end stripped", in: "text\x1b", want: "text"},
		{name: "truncated CSI stripped", in: "a\x1b[31", want: "a"},
		{name: "NUL and bell dropped", in: "a\x00b\x07c", want: "abc"},
		{name: "C1 control dropped", in: "a\u0085b", want: "ab"},
		{name: "DEL dropped", in: "a\x7fb", want: "ab"},
		{name: "DCS string stripped", in: "x\x1bP1;2q data\x1b\\y", want: "xy"},
		{name: "invalid UTF-8 replaced", in: "a\xff\xfeb", want: "a\uFFFD\uFFFDb"},
		{name: "valid UTF-8 preserved", in: "héllo → 世界", want: "héllo → 世界"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Render(tt.in); got != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderLongInput(t *testing.T) {
	in := strings.Repeat("\x1b[31mab\x1b[0m", 1000)
	want := strings.Repeat("ab", 1000)
	if got := Render(in); got != want {
		t.Errorf("Render of repeated ANSI input mismatched")
	}
}

func TestNormalizeField(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{name: "quoted plain text", in: "hello", maxLen: 100, want: `"hello"`},
		{name: "controls removed then quoted", in: "a\x1b[31mb\x00c", maxLen: 100, want: `"abc"`},
		{name: "bounded by runes", in: "ααααα", maxLen: 3, want: `"ααα"`},
		{name: "zero limit keeps all", in: "abc", maxLen: 0, want: `"abc"`},
		{name: "inner quotes escaped", in: `say "hi"`, maxLen: 100, want: `"say \"hi\""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeField(tt.in, tt.maxLen); got != tt.want {
				t.Errorf("NormalizeField(%q, %d) = %s, want %s", tt.in, tt.maxLen, got, tt.want)
			}
		})
	}
}
