package ui

import (
	"bytes"
	"regexp"
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
	plain := regexp.MustCompile(`\x1b\[[0-9;:]*m`).ReplaceAllString(got, "")
	if got == plain {
		t.Fatal("progress title is missing shimmer colors")
	}
	if strings.ContainsRune(plain, '\x1b') {
		t.Fatalf("progress emitted a non-color escape sequence: %q", got)
	}
	if !strings.Contains(plain, "Calculating recommendations 1/43") {
		t.Fatalf("progress text or count missing: %q", plain)
	}
	if !strings.Contains(plain, "⣾ Calculating recommendations") {
		t.Fatalf("progress spinner missing: %q", plain)
	}
	if !strings.HasSuffix(plain, "\r"+strings.Repeat(" ", 2+len("Calculating recommendations 1/43"))+"\r") {
		t.Fatalf("progress did not clear its visible width: %q", plain)
	}
}
