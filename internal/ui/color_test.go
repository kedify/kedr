package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

func TestColorOptOutAlsoAppliesToProgress(t *testing.T) {
	for _, tc := range []struct {
		flag bool
		env  string
	}{{true, ""}, {false, "1"}, {false, "0"}, {false, "false"}, {false, "any-value"}} {
		t.Setenv("NO_COLOR", tc.env)
		t.Setenv("CLICOLOR_FORCE", "1")
		var output bytes.Buffer
		writer := &colorprofile.Writer{Forward: &output, Profile: ColorProfile(os.Stderr, tc.flag)}
		progress := startProgress(1, true, writer)
		progress.Stop()
		if text := output.String(); strings.ContainsRune(text, '\x1b') || !strings.Contains(text, progressTitle) {
			t.Fatalf("no-color=%t NO_COLOR=%q: progress output = %q", tc.flag, tc.env, text)
		}
	}
}

func TestColorDetectionRequiresTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("TTY_FORCE", "1")
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if IsTerminal(file) || ColorProfile(file, false) != colorprofile.NoTTY {
		t.Fatal("redirected output was treated as a color terminal")
	}
}
