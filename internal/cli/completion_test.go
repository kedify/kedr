package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCompletionScripts(t *testing.T) {
	bash, err := execute("completion", "bash")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bash, "__start_kedr") {
		t.Fatal("Bash script does not register kedr completion")
	}
	defaultScript, err := execute("completion")
	if err != nil || defaultScript != bash {
		t.Fatalf("default completion differs from Bash: %v", err)
	}
	zsh, err := execute("completion", "zsh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(zsh, "compdef _kedr kedr") {
		t.Fatal("Zsh script does not register kedr completion")
	}
}

func TestCompletionHelp(t *testing.T) {
	output, err := execute("completion", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"completion [bash|zsh]", "Defaults to Bash",
		`eval "$(kedr completion bash)"`, `eval "$(kedr completion zsh)"`,
		"~/.bashrc", "~/.zshrc", "bash-completion", "autoload -Uz compinit && compinit",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("completion help missing %q", want)
		}
	}
}

func TestCompletionInvalidArgs(t *testing.T) {
	for _, args := range [][]string{{"fish"}, {"bash", "zsh"}} {
		output, err := execute(append([]string{"completion"}, args...)...)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Errorf("completion %v: expected usage error, got %v", args, err)
		}
		if output != "" {
			t.Errorf("completion %v: unexpected output %q", args, output)
		}
	}
}

func TestCompletionSuggestions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"commands", []string{""}, []string{"completion", "explain", "simple", "simple_limit", "version"}},
		{"command prefix", []string{"sim"}, []string{"simple", "simple_limit"}},
		{"shells", []string{"completion", ""}, []string{"bash", "zsh"}},
		{"shell prefix", []string{"completion", "z"}, []string{"zsh"}},
		{"strategy flag", []string{"simple", "--names"}, []string{"--namespace"}},
		{"global flag", []string{"--no-c"}, []string{"--no-color"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := NewRoot()
			var output, stderr bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"__completeNoDesc"}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			for _, want := range tc.want {
				if !slices.Contains(lines, want) {
					t.Errorf("completion missing %q: %s", want, output.String())
				}
			}
			if lines[len(lines)-1] != ":4" {
				t.Errorf("completion should disable file suggestions: %s", output.String())
			}
		})
	}
}
