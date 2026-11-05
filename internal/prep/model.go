// Package prep is the control plane's own model call: the one pass that
// reads a job's goal before its sandbox exists.
//
// It is the only place spov talks to a model itself. Everywhere else a
// provider key is a value spov carries into a sandbox and never uses.
//
// @decision(DL-047)
package prep

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The provider ladder. The first key present decides both the endpoint
// and the model, so a deployment that already has a key for its
// sandboxes gets a working pass with no configuration at all. The
// order is OpenRouter, then Gemini, then OpenAI.
//
// @decision(DL-047)
const (
	OpenRouterKeyEnv = "OPENROUTER_API_KEY"
	GeminiKeyEnv     = "GEMINI_API_KEY"
	OpenAIKeyEnv     = "OPENAI_API_KEY"
)

// The endpoints. An empty base URL means the SDK's own default, which
// is OpenAI's.
//
// @decision(DL-047)
const (
	OpenRouterBaseURL = "https://openrouter.ai/api/v1"
	GeminiBaseURL     = "https://generativelanguage.googleapis.com/v1beta/openai/"
	OpenAIBaseURL     = ""
)

// The fallback models, one per rung, used only when the model list has
// nothing of the rung's family. They are literals that rot: a retired
// model answers 404 and a failed pass does not fail the job, so
// Selection.Describe and the caller's warning split are what make that
// visible rather than silent.
//
// @decision(DL-047)
const (
	OpenRouterModel = "anthropic/claude-sonnet-5"
	GeminiModel     = "gemini-3.8-flash"
	OpenAIModel     = "gpt-5.6-luna"
)

// The rungs' families: a capable, cheap model of each provider, as
// model ids in the list match them.
//
// @decision(DL-047)
var (
	OpenRouterFamily = regexp.MustCompile(`^anthropic/claude-sonnet-[0-9][0-9.]*$`)
	GeminiFamily     = regexp.MustCompile(`^gemini-[0-9][0-9.]*-flash$`)
	OpenAIFamily     = regexp.MustCompile(`^gpt-[0-9][0-9.]*-luna$`)
)

// Newest finds the newest model of a provider in a family, "" when the
// list has none. modelcat.List.Newest is one.
//
// @decision(DL-047)
type Newest func(provider string, family *regexp.Regexp) string

// Config is the deployment's explicit override, read only by
// `spov serve`. Every field is optional; an empty Config means the
// ladder decides everything. It is deployment-level rather than
// per-user or per-job: a per-job model would mean goal text
// influencing which model reads the goal text.
//
// @decision(DL-047)
type Config struct {
	// BaseURL points at an OpenAI-compatible endpoint: a LiteLLM-style
	// gateway, a proxy, or another provider entirely. Setting it
	// requires Model, because spov cannot know a gateway's catalogue.
	BaseURL string `koanf:"base_url" json:"base_url,omitempty" yaml:"base_url,omitempty"`
	// Model overrides the model name alone, keeping whatever endpoint
	// the ladder detected. This is the common "OpenRouter, but a
	// cheaper model" case.
	Model string `koanf:"model" json:"model,omitempty" yaml:"model,omitempty"`
	// KeyEnv names the environment variable holding the key. It is also
	// the seam a deployment uses to bill the control plane's calls
	// separately from its sandboxes': naming a variable nothing else
	// reads keeps the two budgets apart.
	KeyEnv string `koanf:"key_env" json:"key_env,omitempty" yaml:"key_env,omitempty"`
}

// Selection is a resolved provider: where to send the request, which
// model to ask for, and the key to use.
//
// @decision(DL-047)
type Selection struct {
	BaseURL string
	Model   string
	Key     string
	// KeyEnv is the variable the key came from, for the log line. The
	// key itself is never logged.
	KeyEnv string
	// rung is the ladder rung that chose the model, nil when the model
	// was configured; Reselect picks again from it.
	rung *rung
}

// Reselect is sel with its rung's model picked again from the list, so a
// pass follows the list as it refreshes without reading its key again.
// A configured model, or no pick, leaves sel as it is.
//
// @decision(DL-047)
func Reselect(sel Selection, newest Newest) Selection {
	if sel.rung == nil || newest == nil {
		return sel
	}
	if picked := newest(sel.rung.provider, sel.rung.family); picked != "" {
		sel.Model = picked
	}
	return sel
}

// Describe names the endpoint and model without revealing the key. It
// is what the startup log says.
//
// @decision(DL-047)
func (s Selection) Describe() string {
	where := s.BaseURL
	if where == "" {
		where = "the OpenAI API"
	}
	return fmt.Sprintf("%s at %s (key from %s)", s.Model, where, s.KeyEnv)
}

// ErrNoProvider means no key was found and none was configured. It is
// an ordinary zero-configuration state rather than a fault: the pass is
// skipped and the job opens on its goal alone.
//
// Callers must keep it distinct from a call that was attempted and
// failed. A retired default model produces the latter, and if both read
// the same in the log then a deployment whose model went away looks
// exactly like one that never configured a key.
//
// @decision(DL-047)
var ErrNoProvider = errors.New("prep: no model provider key is set")

// rung is one step of the ladder.
type rung struct {
	env      string
	baseURL  string
	provider string
	family   *regexp.Regexp
	model    string
}

// ladder is the detection order. It stops at three deliberately: each
// further rung is a base URL and a model literal ageing on its own
// schedule, and anyone holding only a further-down key can set Config.
//
// @decision(DL-047)
var ladder = []rung{
	{OpenRouterKeyEnv, OpenRouterBaseURL, "openrouter", OpenRouterFamily, OpenRouterModel},
	{GeminiKeyEnv, GeminiBaseURL, "google", GeminiFamily, GeminiModel},
	{OpenAIKeyEnv, OpenAIBaseURL, "openai", OpenAIFamily, OpenAIModel},
}

// Resolve picks a provider from the configuration and a key lookup. The
// lookup is passed in rather than reading the environment directly, so
// a test never depends on the shared process environment; the caller
// layers environment and credential store the same way `providerKeys`
// does for a sandbox (DL-008).
//
// The rung's model is the newest of its family in the list, else its
// literal; newest may be nil.
//
// @decision(DL-047) @decision(DL-008)
func Resolve(cfg Config, lookup func(string) string, newest Newest) (Selection, error) {
	if lookup == nil {
		return Selection{}, fmt.Errorf("prep: Resolve needs a key lookup")
	}
	cfg = Config{
		BaseURL: strings.TrimSpace(cfg.BaseURL),
		Model:   strings.TrimSpace(cfg.Model),
		KeyEnv:  strings.TrimSpace(cfg.KeyEnv),
	}
	// A base URL with no model is refused rather than guessed at. spov
	// cannot know what a gateway serves, and a guessed model name is
	// the guessed-check problem in another costume: it either 404s on
	// every job or silently asks for something nobody chose.
	if cfg.BaseURL != "" && cfg.Model == "" {
		return Selection{}, fmt.Errorf("prep: base_url needs a model, since spov cannot know what an endpoint serves")
	}

	// An explicitly named key variable is authoritative: it is how a
	// deployment separates this budget from its sandboxes'. If it is
	// named and empty, that is a configuration error and not a reason
	// to fall down the ladder to a key meant for something else.
	if cfg.KeyEnv != "" {
		key := strings.TrimSpace(lookup(cfg.KeyEnv))
		if key == "" {
			return Selection{}, fmt.Errorf("prep: %s is configured as the key but is empty", cfg.KeyEnv)
		}
		if cfg.Model == "" {
			return Selection{}, fmt.Errorf("prep: key_env needs a model, since the key does not say which provider it is for")
		}
		return Selection{BaseURL: cfg.BaseURL, Model: cfg.Model, Key: key, KeyEnv: cfg.KeyEnv}, nil
	}

	// A configured base URL with no key variable still needs a key, and
	// the ladder is where it comes from.
	for _, r := range ladder {
		key := strings.TrimSpace(lookup(r.env))
		if key == "" {
			continue
		}
		model := r.model
		if newest != nil {
			if picked := newest(r.provider, r.family); picked != "" {
				model = picked
			}
		}
		sel := Selection{BaseURL: r.baseURL, Model: model, Key: key, KeyEnv: r.env, rung: &r}
		// Config overrides what the rung decided, per field: a model
		// alone keeps the detected endpoint.
		if cfg.BaseURL != "" {
			sel.BaseURL = cfg.BaseURL
		}
		if cfg.Model != "" {
			sel.Model = cfg.Model
			sel.rung = nil
		}
		return sel, nil
	}
	return Selection{}, ErrNoProvider
}
