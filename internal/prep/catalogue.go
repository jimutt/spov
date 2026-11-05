package prep

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/credproxy"
)

// Catalogue is the providers spov forwards a key for, and where the
// proxy uses each key. Their models are models.dev's, in
// internal/modelcat; this list never follows a download.
//
// @decision(DL-013) @decision(DL-034)
func Catalogue() []Provider {
	return []Provider{
		{
			ID: "anthropic", KeyEnv: "ANTHROPIC_API_KEY", Name: "Anthropic",
			API: credproxy.ProviderAPI{Host: "api.anthropic.com", Header: credproxy.HeaderAPIKey},
		},
		{
			ID: "openai", KeyEnv: "OPENAI_API_KEY", Name: "OpenAI",
			API: credproxy.ProviderAPI{Host: "api.openai.com", Header: credproxy.HeaderBearer},
		},
		{
			// One key reaches every family, which is why DL-047 puts it
			// at the head of the ladder.
			ID: "openrouter", KeyEnv: "OPENROUTER_API_KEY", Name: "OpenRouter",
			API: credproxy.ProviderAPI{Host: "openrouter.ai", Header: credproxy.HeaderBearer},
		},
		{
			// The provider ID is `google`, not `gemini`, and the key
			// variable is GEMINI_API_KEY. The two do not match and
			// neither is a typo.
			ID: "google", KeyEnv: "GEMINI_API_KEY", Name: "Google Gemini",
			API: credproxy.ProviderAPI{Host: "generativelanguage.googleapis.com", Header: credproxy.HeaderGoogAPIKey},
		},
		{
			ID: "groq", KeyEnv: "GROQ_API_KEY", Name: "Groq",
			API: credproxy.ProviderAPI{Host: "api.groq.com", Header: credproxy.HeaderBearer},
		},
		{
			ID: "xai", KeyEnv: "XAI_API_KEY", Name: "xAI",
			API: credproxy.ProviderAPI{Host: "api.x.ai", Header: credproxy.HeaderBearer},
		},
		{
			ID: "deepseek", KeyEnv: "DEEPSEEK_API_KEY", Name: "DeepSeek",
			API: credproxy.ProviderAPI{Host: "api.deepseek.com", Header: credproxy.HeaderBearer},
		},
		{
			// OpenCode Go, the subscription. It and Zen below share one
			// key variable and one API: the user's OpenCode Console
			// credential, which OpenCode itself routes per model.
			ID: "opencode-go", KeyEnv: OpenCodeKeyEnv, Name: "OpenCode Go",
			API: openCodeAPI,
		},
		{
			ID: "opencode", KeyEnv: OpenCodeKeyEnv, Name: "OpenCode Zen",
			API: openCodeAPI,
		},
		{
			ID: "mistral", KeyEnv: "MISTRAL_API_KEY", Name: "Mistral",
			API: credproxy.ProviderAPI{Host: "api.mistral.ai", Header: credproxy.HeaderBearer},
		},
	}
}

// OpenCodeKeyEnv is the variable OpenCode reads its OpenCode Console
// credential from: a pasted key, or the access token of a sign-in spov
// holds.
//
// @decision(DL-015)
const OpenCodeKeyEnv = "OPENCODE_API_KEY"

// openCodeAPI is where the console credential is used: the console
// config OpenCode loads its providers from, the model gateway that config
// points at, Zen's public API (neither a sign-in nor a pasted key has been
// seen to use it), and web search. No other
// console path gets it, and the config gets it only to be read.
//
// @decision(DL-015)
var openCodeAPI = credproxy.ProviderAPI{
	Host: "opencode.ai", Header: credproxy.HeaderBearer,
	Paths: []string{"GET /console/api/v2/config", "/inference/", "/zen/", "/console/api/websearch"},
}

// Provider is one model provider spov can carry a key for.
//
// @decision(DL-013)
type Provider struct {
	// ID is OpenCode's own provider identifier — the half of
	// `provider/model` before the slash. It is read out of the pinned
	// build and is not always the obvious word: Gemini's is `google`.
	ID string
	// KeyEnv is the environment variable the provider's key lives in,
	// which is also what `spov credentials add` stores it under.
	KeyEnv string
	// Name is for a human reading a list.
	Name string
	// API is the provider's API host at OpenCode's default base URL, and
	// the header its key goes in. The proxy sets the key there and
	// nowhere else.
	//
	// @decision(DL-034)
	API credproxy.ProviderAPI
}

// Ref is the `provider/model` string DL-014's field takes.
//
// @decision(DL-013) @decision(DL-014)
func (p Provider) Ref(model string) string { return p.ID + "/" + model }

// ProviderKeyEnvs are the variables spov forwards into a sandbox, in
// catalogue order.
//
// @decision(DL-013)
func ProviderKeyEnvs() []string {
	c := Catalogue()
	envs := make([]string, 0, len(c))
	for _, p := range c {
		if !slices.Contains(envs, p.KeyEnv) {
			envs = append(envs, p.KeyEnv)
		}
	}
	return envs
}

// ProviderAPIs names, for each forwarded key variable, where the proxy
// uses the key.
//
// @decision(DL-034)
func ProviderAPIs() map[string]credproxy.ProviderAPI {
	c := Catalogue()
	apis := make(map[string]credproxy.ProviderAPI, len(c))
	for _, p := range c {
		apis[p.KeyEnv] = p.API
	}
	return apis
}

// The two ways a model reference fails to name a provider spov carries,
// kept apart because their fixes differ. Whether the model under it is
// listed is modelcat's question.
//
// @decision(DL-013)
var (
	ErrNotAReference  = errors.New("prep: not a provider/model reference")
	ErrNoSuchProvider = errors.New("prep: no such provider")
)

// LookupModel resolves a `provider/model[#variant]` reference to the
// provider it names and the model half with any variant removed. A
// model id may itself contain slashes (OpenRouter's all do), so it
// splits on the FIRST slash only. The `#variant` suffix must agree with
// `modelRef` in internal/server, which parses it the same way.
//
// @decision(DL-013)
func LookupModel(ref string) (Provider, string, error) {
	id, rest, ok := strings.Cut(ref, "/")
	if !ok || id == "" || rest == "" {
		return Provider{}, "", fmt.Errorf("%w: %q", ErrNotAReference, ref)
	}
	model, variant, hasVariant := strings.Cut(rest, "#")
	if hasVariant && (model == "" || variant == "") {
		return Provider{}, "", fmt.Errorf("%w: %q", ErrNotAReference, ref)
	}
	for _, p := range Catalogue() {
		if p.ID == id {
			return p, model, nil
		}
	}
	return Provider{}, model, fmt.Errorf("%w: %q", ErrNoSuchProvider, id)
}
