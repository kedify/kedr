package cli

import (
	"bytes"
	"strings"
	"testing"
)

func execute(args ...string) (string, error) {
	cmd := NewRoot()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return output.String(), err
}
func TestHelpAndVersion(t *testing.T) {
	output, err := execute("simple", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--cpu-percentile", "--prometheus-url", "--job-grouping-labels", "--fileoutput"} {
		if !strings.Contains(output, flag) {
			t.Errorf("help missing %s", flag)
		}
	}
	output, err = execute("version")
	if err != nil || strings.TrimSpace(output) != "dev" {
		t.Fatalf("version=%q err=%v", output, err)
	}
}
func TestRemovedFlagsAreUnknown(t *testing.T) {
	for _, flag := range []string{"--slackoutput", "--azurebloboutput", "--publish_scan_url"} {
		_, err := execute("simple", flag, "x")
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%s: %v", flag, err)
		}
	}
}
func TestUnderscoreStrategyFlagsAreAccepted(t *testing.T) {
	_, err := execute("simple", "--history_duration", "0")
	if err == nil || strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected validation rather than parse error: %v", err)
	}
}
