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
	for _, flag := range []string{"--cpu-percentile", "--prometheus-url", "--job-grouping-labels", "--fileoutput", "--minimum-history-hours", "--detect-memory-leaks", "--release-history", "--explain", "--full"} {
		if !strings.Contains(output, flag) {
			t.Errorf("help missing %s", flag)
		}
	}
	output, err = execute("version")
	if err != nil || strings.TrimSpace(output) != "dev" {
		t.Fatalf("version=%q err=%v", output, err)
	}
}

func TestRolloutEvidenceDefaults(t *testing.T) {
	root := NewRoot()
	for _, name := range []string{"simple", "simple_limit"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Flags().Lookup("minimum-history-hours").DefValue != "1" || cmd.Flags().Lookup("points-required").DefValue != "30" || cmd.Flags().Lookup("release-history").DefValue != "4" {
			t.Fatal("CLI defaults do not match one hour, 30 observations and current plus three previous rollouts")
		}
		if flag := cmd.Flags().Lookup("full"); flag == nil || flag.DefValue != "false" {
			t.Fatal("full table output should be available and disabled by default")
		}
		if flag := cmd.Flags().Lookup("history-duration-hours"); flag == nil || flag.DefValue != "48" {
			t.Fatal("history-duration-hours should default to 48 hours")
		}
	}
}

func TestCPULimitPercentileMigration(t *testing.T) {
	for _, flag := range []string{"--cpu-limit", "--cpu_limit"} {
		_, err := execute("simple_limit", flag, "96")
		if err == nil || !strings.Contains(err.Error(), "--cpu-limit-ratio") {
			t.Fatalf("missing migration error: %v", err)
		}
	}
	text, err := execute("simple_limit", "--help")
	if err != nil || !strings.Contains(text, "--cpu-limit-ratio") {
		t.Fatalf("ratio flag missing: %v", err)
	}
}
func TestRemovedFlagsAreUnknown(t *testing.T) {
	for _, flag := range []string{"--slackoutput", "--azurebloboutput", "--publish_scan_url", "--history-duration", "--history_duration"} {
		_, err := execute("simple", flag, "x")
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("%s: %v", flag, err)
		}
	}
}
func TestUnderscoreStrategyFlagsAreAccepted(t *testing.T) {
	_, err := execute("simple", "--history_duration_hours", "0")
	if err == nil || strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected validation rather than parse error: %v", err)
	}
}
