package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
)

func TestSetup(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, log.InfoLevel)
	if logger == nil {
		t.Fatal("Setup returned nil logger")
	}
	logger.Info("daemon started", "port", 41001)
	out := buf.String()
	if !strings.Contains(out, "daemon started") {
		t.Errorf("log output %q missing message", out)
	}
	if !strings.Contains(out, "41001") {
		t.Errorf("log output %q missing structured field", out)
	}
	if !strings.Contains(out, "INFO") {
		t.Errorf("log output %q missing level", out)
	}
}

func TestSetupLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, log.ErrorLevel)
	logger.Info("should be dropped")
	if buf.Len() != 0 {
		t.Errorf("info message passed error-level logger: %q", buf.String())
	}
	logger.Error("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Error("error message dropped by error-level logger")
	}
}

func TestAgentField(t *testing.T) {
	got := AgentField("hi\x1b[31mthere\x1b[0m", 100)
	want := `"hithere"`
	if got != want {
		t.Errorf("AgentField = %s, want %s", got, want)
	}
	if got2 := AgentField(strings.Repeat("α", 10), 4); got2 != `"αααα"` {
		t.Errorf("AgentField bound failed: %s", got2)
	}
}

func TestAgentFieldUsedAsFieldValue(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, log.InfoLevel)
	logger.Info("note accepted", "body", AgentField("\x1b]0;evil\x07body text", 100))
	if strings.Contains(buf.String(), "evil\x1b") {
		t.Errorf("raw escape leaked into log: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "body text") {
		t.Errorf("normalized field missing: %q", buf.String())
	}
}
