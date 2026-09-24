package ui

import (
	"os"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
)

// IsTerminal checks the actual file descriptor, including redirected output.
func IsTerminal(file *os.File) bool { return term.IsTerminal(file.Fd()) }

// ColorProfile respects explicit color opt-outs before terminal detection.
// NO_COLOR disables colors for any nonempty value, including "0" or "false".
func ColorProfile(file *os.File, noColor bool) colorprofile.Profile {
	if noColor || os.Getenv("NO_COLOR") != "" || !IsTerminal(file) || os.Getenv("TERM") == "dumb" {
		return colorprofile.NoTTY
	}
	return colorprofile.Detect(file, os.Environ())
}
