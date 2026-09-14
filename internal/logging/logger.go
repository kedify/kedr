// Package logging provides small, redaction-safe CLI logging.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kedify/kedr/internal/config"
)

type Logger struct {
	out                  io.Writer
	verbose, quiet, json bool
	mu                   sync.Mutex
}

func New(cfg *config.Config) *Logger {
	out := io.Writer(os.Stdout)
	if cfg.LogToStderr {
		out = os.Stderr
	}
	jsonLogs := false
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ENABLE_JSON_LOGS_FORMAT"))) {
	case "true", "1", "yes":
		jsonLogs = true
	}
	return &Logger{out: out, verbose: cfg.Verbose, quiet: cfg.Quiet, json: jsonLogs}
}
func (l *Logger) write(level, format string, args ...any) {
	if l.quiet {
		return
	}
	if level == "DEBUG" && !l.verbose {
		return
	}
	message := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.json {
		data, _ := json.Marshal(map[string]any{"asctime": time.Now().Format("2006-01-02T15:04:05"), "severity": level, "name": "kedr", "message": message})
		_, _ = fmt.Fprintln(l.out, string(data))
		return
	}
	_, _ = fmt.Fprintf(l.out, "%s\n", message)
}
func (l *Logger) Debugf(f string, a ...any) { l.write("DEBUG", f, a...) }
func (l *Logger) Infof(f string, a ...any)  { l.write("INFO", f, a...) }
func (l *Logger) Warnf(f string, a ...any)  { l.write("WARNING", f, a...) }
func (l *Logger) Errorf(f string, a ...any) { l.write("ERROR", f, a...) }
