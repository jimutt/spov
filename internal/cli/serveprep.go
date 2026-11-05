package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/server"
)

// prepareGoal builds the supervisor's preparation seam from the
// deployment's configuration and environment. It returns nil when no
// provider is configured, an ordinary zero-configuration state said
// once at startup; a non-nil seam that fails per job is a fault, warned
// about each time by the supervisor. The key is resolved once; each pass
// picks its rung's model again, so it follows the model list.
//
// @decision(DL-047)
func (e *Env) prepareGoal(cfg prep.Config, models server.ModelCatalogue, warn func(string)) func(context.Context, string) (prep.Assessment, error) {
	newest := func(provider string, family *regexp.Regexp) string {
		if models == nil {
			return ""
		}
		list, _ := models.Models()
		return list.Newest(provider, family)
	}
	sel, err := prep.Resolve(cfg, e.prepKey, newest)
	if err != nil {
		if errors.Is(err, prep.ErrNoProvider) {
			e.note("no model provider key is configured, so a job's goal is not prepared before it starts" +
				" (set OPENROUTER_API_KEY, GEMINI_API_KEY or OPENAI_API_KEY, or configure prep.*)")
			return nil
		}
		// A misconfiguration rather than an absence: something was
		// asked for and could not be built. It is not fatal — a job
		// still runs on its goal — but it is not the quiet path either.
		warn("the goal preparation pass is not configured correctly, so no job's goal will be prepared: " + err.Error())
		return nil
	}
	if _, err := prep.NewClient(sel); err != nil {
		warn("the goal preparation pass could not be built, so no job's goal will be prepared: " + err.Error())
		return nil
	}
	// The model and endpoint are named at startup; the key never is.
	e.note("a job's goal is prepared by " + sel.Describe())
	return func(ctx context.Context, goal string) (prep.Assessment, error) {
		client, err := prep.NewClient(prep.Reselect(sel, newest))
		if err != nil {
			return prep.Assessment{}, err
		}
		return prep.Pass(ctx, client, goal)
	}
}

// note writes an informational startup line, in the same shape as the
// warnings beside it.
func (e *Env) note(msg string) { _, _ = fmt.Fprintln(e.Err, "spov serve: "+msg) }

// prepKey finds the key for one variable name: the environment first,
// then the credential store, the same layering `providerKeys` does for
// a sandbox. A store that cannot be read is not an error here: the
// environment may well have answered.
//
// @decision(DL-047) @decision(DL-008)
func (e *Env) prepKey(name string) string {
	if v := strings.TrimSpace(e.Environ.Getenv(name)); v != "" {
		return v
	}
	v, err := e.storedProviderKey(name)
	if err != nil {
		return ""
	}
	return v
}
