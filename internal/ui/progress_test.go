package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestProgressDoesNotEmitTerminalModeEscapes(t *testing.T) {
	var output bytes.Buffer
	progress := startProgress(43, true, &output)
	progress.Increment()
	time.Sleep(120 * time.Millisecond)
	progress.Stop()
	progress.Stop()

	got := output.String()
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("progress emitted an escape sequence: %q", got)
	}
	if !strings.Contains(got, "Calculating recommendations") {
		t.Fatalf("progress text missing: %q", got)
	}
}
