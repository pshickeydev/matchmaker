// Package logging configures Matchmaker's structured logging on
// charmbracelet/log (DESIGN §10.1) and enforces the agent-content rules
// of DESIGN §5.5 and THREAT_MODEL §5.4:
//
//   - agent-controlled text is never interpolated into message strings;
//     when necessary it is emitted as a quoted, length-bounded field after
//     control/escape normalization (render.NormalizeField)
//   - raw Crush payloads, full prompts, note bodies, and run output are
//     never logged.
package logging

import (
	"io"
	"time"

	"github.com/charmbracelet/log"

	"github.com/pshickeydev/matchmaker/internal/render"
)

// timeFormat is the log timestamp layout.
const timeFormat = time.DateTime

// Setup installs the process-wide logger writing to w at the given level
// and returns it.
func Setup(w io.Writer, level log.Level) *log.Logger {
	logger := log.New(w)
	logger.SetLevel(level)
	logger.SetReportTimestamp(true)
	logger.SetTimeFormat(timeFormat)
	log.SetDefault(logger)
	return logger
}

// AgentField prepares one structured log field value from
// agent-controlled text: the value is control/escape-normalized,
// length-bounded, and quoted (DESIGN §5.5). It is passed to
// logger.With(key, value), never used as a message string.
func AgentField(agentText string, maxLen int) string {
	return render.NormalizeField(agentText, maxLen)
}
