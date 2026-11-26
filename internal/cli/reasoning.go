package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
)

// addReasoningFlag adds --reasoning: the mode of the model the launch
// resolves to, --model's or the account default's, sent as a field of
// its own that the server checks.
//
// @decision(DL-190)
func addReasoningFlag(env *Env, cmd *cobra.Command, v *string) {
	cmd.Flags().StringVar(v, "reasoning", "",
		"reasoning mode of the model, e.g. high, as GET /api/v1/models offers it for --model or your default model;"+
			" default for the model's own")
	_ = cmd.RegisterFlagCompletionFunc("reasoning", completeReasoning(env))
}

// checkReasoning refuses --reasoning beside a --model that names a mode
// already, before the server is asked.
//
// @decision(DL-190)
func checkReasoning(model, reasoning string) error {
	if strings.TrimSpace(reasoning) != "" && strings.Contains(model, "#") {
		return errors.New("--model names a reasoning mode already; give it in --model or in --reasoning, not both")
	}
	return nil
}

// completeReasoning offers the modes of the --model given, and default,
// from a server already up; nothing without --model.
//
// @decision(DL-190) @decision(DL-023)
func completeReasoning(env *Env) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		model, _ := cmd.Flags().GetString("model")
		model = strings.TrimSpace(model)
		if model == "" || strings.Contains(model, "#") {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
			c, ok := env.existingClient(ctx)
			if !ok {
				return nil
			}
			providers, err := c.ListModels(ctx)
			if err != nil {
				return nil
			}
			var out []cobra.Completion
			for _, mode := range append(modesFor(providers, model), server.DefaultReasoning) {
				if strings.HasPrefix(mode, prefix) {
					out = append(out, mode)
				}
			}
			return out
			// A flag's value completes whatever the command's arguments.
		})(cmd, nil, toComplete)
	}
}

// modesFor is a model's modes in the model list, in OpenCode's order.
func modesFor(providers []server.ModelProviderView, model string) []string {
	for _, p := range providers {
		if modes, ok := p.Variants[model]; ok {
			return modes
		}
	}
	return nil
}
