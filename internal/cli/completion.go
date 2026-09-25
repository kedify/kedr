package cli

import "github.com/spf13/cobra"

func completionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh]",
		Short: "Generate shell completion for Bash or Zsh",
		Long: `Generate a shell completion script for kedr commands and flags.
Defaults to Bash when no shell is specified.

Installation:
  Bash requires the bash-completion package to be installed and loaded in your
  shell. Install it with your OS's package manager if needed.

  Load completion in the current Bash session:
    eval "$(kedr completion bash)"

  Load completion in the current Zsh session:
    eval "$(kedr completion zsh)"

  To enable completion in every session, add the corresponding eval command
  to ~/.bashrc or ~/.zshrc.

  Zsh's completion system must be initialized before loading the script. If it
  is not already enabled by your shell configuration, run the following first
  (and place it before the eval command in ~/.zshrc):
    autoload -Uz compinit && compinit`,
		ValidArgs: []string{"bash", "zsh"},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs)(cmd, args); err != nil {
				return usageError{err}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] == "zsh" {
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			}
			return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
		},
	}
}
