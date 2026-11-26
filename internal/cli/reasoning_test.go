package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// listedAnthropicModel is a model the fixture's shipped list carries.
func listedAnthropicModel(t *testing.T) string {
	t.Helper()
	models := modelcat.Snapshot().Providers["anthropic"]
	if len(models) == 0 {
		t.Fatal("the shipped list carries no anthropic model")
	}
	return "anthropic/" + models[0].ID
}

// --reasoning completes the modes the server offers for the --model
// given, then default, and nothing without --model; a job's goal
// argument does not stop it.
//
// @decision(DL-190)
func TestReasoningCompletesTheModelsModes(t *testing.T) {
	f := newAPIFixture(t)
	model := listedAnthropicModel(t)
	if err := f.deps.Store.ModelModes().Keep(context.Background(), auth.LocalID, f.deps.Defaults.OpenCodeVersion,
		map[string][]string{model: {"low", "high"}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(completions(t, f.env, "fly", "--model", model, "--reasoning", ""), " "); got != "low high default" {
		t.Errorf("fly --reasoning offers %q", got)
	}
	if got := strings.Join(completions(t, f.env, "jobs", "create", "a goal", "--model", model, "--reasoning", "h"), " "); got != "high" {
		t.Errorf("jobs create --reasoning h offers %q", got)
	}
	if got := completions(t, f.env, "fly", "--reasoning", ""); len(got) != 0 {
		t.Errorf("without --model --reasoning offers %v", got)
	}
}

// --reasoning reaches a job's row on the model it names, and is refused
// before the server is asked beside a --model that names a mode.
//
// @decision(DL-190)
func TestJobsCreateSendsTheReasoning(t *testing.T) {
	testRunFlagReachesTheRow(t, []string{"--model", "anthropic/claude-sonnet-5", "--reasoning", "high"},
		func(t *testing.T, j store.Job) {
			if j.Model != "anthropic/claude-sonnet-5#high" {
				t.Errorf("the row carries model %q, want the mode on it", j.Model)
			}
		})
	f := newAPIFixture(t)
	_, _, err := runWith(t, f.env, "jobs", "create", "a goal", "--repo", "alice/spov",
		"--model", "anthropic/claude-sonnet-5#low", "--reasoning", "high")
	if err == nil || !strings.Contains(err.Error(), "--model names a reasoning mode") {
		t.Errorf("--reasoning beside a --model with a mode: %v", err)
	}
	if err := checkReasoning("p/m#low", ""); err != nil {
		t.Errorf("a --model with a mode alone was refused: %v", err)
	}
}

// fly carries --reasoning in its request, trimmed, and nothing without.
//
// @decision(DL-190)
func TestFlyRequestCarriesTheReasoning(t *testing.T) {
	cfg := config.Defaults()
	refs := []server.RepoRef{{Platform: "p", Owner: "o", Name: "r"}}
	if got := sessionRequest(cfg, flyOptions{Reasoning: " high "}, "n", refs, gitIdentityOf("A", "a@x")); got.Reasoning != "high" {
		t.Errorf("reasoning = %q, want high", got.Reasoning)
	}
	if got := sessionRequest(cfg, flyOptions{}, "n", refs, gitIdentityOf("A", "a@x")); got.Reasoning != "" {
		t.Errorf("an unset reasoning became %q", got.Reasoning)
	}
}
