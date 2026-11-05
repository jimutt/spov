package prep

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// env builds a getenv from a map, so no test depends on the process
// environment — which is shared and order-dependent.
func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// The ladder is what makes a zero-configuration deployment work, and
// its ORDER is the decision rather than an implementation detail.
//
// @decision(DL-047)
func TestTheLadderPicksTheFirstKeyItFinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		environ map[string]string
		want    Selection
	}{
		{
			"openrouter first, because one key reaches every model",
			map[string]string{OpenRouterKeyEnv: "k"},
			Selection{BaseURL: OpenRouterBaseURL, Model: OpenRouterModel, Key: "k", KeyEnv: OpenRouterKeyEnv},
		},
		{
			"gemini second",
			map[string]string{GeminiKeyEnv: "k"},
			Selection{BaseURL: GeminiBaseURL, Model: GeminiModel, Key: "k", KeyEnv: GeminiKeyEnv},
		},
		{
			"openai last, on the SDK's own endpoint",
			map[string]string{OpenAIKeyEnv: "k"},
			Selection{BaseURL: "", Model: OpenAIModel, Key: "k", KeyEnv: OpenAIKeyEnv},
		},
		{
			"openrouter outranks the others when several are set",
			map[string]string{OpenAIKeyEnv: "a", GeminiKeyEnv: "b", OpenRouterKeyEnv: "c"},
			Selection{BaseURL: OpenRouterBaseURL, Model: OpenRouterModel, Key: "c", KeyEnv: OpenRouterKeyEnv},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(Config{}, env(tc.environ), nil)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			// The rung is Reselect's, compared apart.
			if got.rung == nil {
				t.Error("a ladder pick carries no rung to pick again from")
			}
			got.rung = nil
			if got != tc.want {
				t.Errorf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// No key is an ordinary state and must be distinguishable from a
// failure, because the caller logs the two differently: a deployment
// whose model was retired must not look like one that never set a key.
//
// @decision(DL-047)
func TestNoKeyIsItsOwnSentinelAndNotAGenericError(t *testing.T) {
	_, err := Resolve(Config{}, env(nil), nil)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Resolve with no keys = %v, want ErrNoProvider", err)
	}
}

// A key spov forwards into sandboxes but does not speak to itself must
// not be mistaken for one it can use. Eight provider variables are
// forwarded; three are on the ladder.
//
// @decision(DL-047)
func TestAKeyThatIsNotOnTheLadderIsNotAProvider(t *testing.T) {
	_, err := Resolve(Config{}, env(map[string]string{
		"ANTHROPIC_API_KEY": "k",
		"GROQ_API_KEY":      "k",
		"DEEPSEEK_API_KEY":  "k",
	}), nil)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Resolve = %v, want ErrNoProvider: spov speaks the OpenAI wire format"+
			" and cannot use a key for an endpoint it does not talk to", err)
	}
}

// A whitespace-only key is not a key. Environment variables acquire
// these from shell quoting and a deployment file.
//
// @decision(DL-047)
func TestAnEmptyKeyIsNotAKey(t *testing.T) {
	_, err := Resolve(Config{}, env(map[string]string{OpenRouterKeyEnv: "   "}), nil)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Resolve with a blank key = %v, want ErrNoProvider", err)
	}
}

// The common override: a different model on the endpoint the ladder
// already found.
//
// @decision(DL-047)
func TestAModelAloneKeepsTheDetectedEndpoint(t *testing.T) {
	got, err := Resolve(Config{Model: "anthropic/claude-haiku-4.5"},
		env(map[string]string{OpenRouterKeyEnv: "k"}), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.BaseURL != OpenRouterBaseURL {
		t.Errorf("BaseURL = %q, want the detected %q", got.BaseURL, OpenRouterBaseURL)
	}
	if got.Model != "anthropic/claude-haiku-4.5" {
		t.Errorf("Model = %q, want the configured one", got.Model)
	}
}

// The gateway case: a LiteLLM-style endpoint reaches Anthropic's own
// models without spov speaking Anthropic's API.
//
// @decision(DL-047)
func TestAGatewayIsReachedByPointingTheBaseURLAtIt(t *testing.T) {
	got, err := Resolve(
		Config{BaseURL: "https://litellm.internal/v1", Model: "claude-sonnet-5", KeyEnv: "LITELLM_API_KEY"},
		env(map[string]string{"LITELLM_API_KEY": "k"}), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Selection{BaseURL: "https://litellm.internal/v1", Model: "claude-sonnet-5",
		Key: "k", KeyEnv: "LITELLM_API_KEY"}
	if got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

// TestABaseURLWithoutAModelIsRefused pins that spov never guesses a
// model for an unknown gateway's catalogue.
//
// @decision(DL-047)
func TestABaseURLWithoutAModelIsRefused(t *testing.T) {
	_, err := Resolve(Config{BaseURL: "https://gateway.internal/v1"},
		env(map[string]string{OpenRouterKeyEnv: "k"}), nil)
	if err == nil {
		t.Fatal("Resolve accepted a base URL with no model; spov cannot know what an endpoint serves")
	}
	if errors.Is(err, ErrNoProvider) {
		t.Error("a misconfiguration reported as ErrNoProvider would be logged as an ordinary" +
			" zero-config state, which is the one distinction the caller depends on")
	}
}

// TestAnEmptyConfiguredKeyDoesNotFallBackToTheLadder pins that a named
// but empty key_env is an error, never a fall-back to another budget.
//
// @decision(DL-047)
func TestAnEmptyConfiguredKeyDoesNotFallBackToTheLadder(t *testing.T) {
	_, err := Resolve(
		Config{Model: "m", KeyEnv: "SPOV_PREP_KEY"},
		env(map[string]string{"SPOV_PREP_KEY": "", OpenRouterKeyEnv: "someone-elses-budget"}), nil)
	if err == nil {
		t.Fatal("Resolve fell back to another key when the configured one was empty")
	}
	if strings.Contains(err.Error(), "someone-elses-budget") {
		t.Error("the error names the key value")
	}
}

// Describe is the startup line. The key must never be in it.
//
// @decision(DL-047)
func TestDescribeNamesTheModelAndNeverTheKey(t *testing.T) {
	sel := Selection{BaseURL: OpenRouterBaseURL, Model: OpenRouterModel,
		Key: "sk-super-secret", KeyEnv: OpenRouterKeyEnv}
	got := sel.Describe()
	if strings.Contains(got, "sk-super-secret") {
		t.Fatalf("Describe leaked the key: %q", got)
	}
	for _, want := range []string{OpenRouterModel, OpenRouterBaseURL, OpenRouterKeyEnv} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, missing %q", got, want)
		}
	}
}

// The OpenAI rung has no base URL, and a startup line reading
// "at " with nothing after it says less than it should.
//
// @decision(DL-047)
func TestDescribeNamesTheDefaultEndpoint(t *testing.T) {
	got := Selection{Model: OpenAIModel, KeyEnv: OpenAIKeyEnv}.Describe()
	if strings.Contains(got, "at  ") || strings.HasSuffix(got, "at ") {
		t.Errorf("Describe = %q, which names no endpoint", got)
	}
}

// Resolve must not read the process environment. A test that passed
// only because the developer happened to have a key exported would fail
// in CI, and worse, a Resolve reading os.Getenv directly could pick up a
// key nobody meant to spend.
//
// @decision(DL-047)
func TestResolveReadsOnlyTheGetenvItWasGiven(t *testing.T) {
	t.Setenv(OpenRouterKeyEnv, "from-the-process")
	_, err := Resolve(Config{}, env(nil), nil)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Resolve = %v; it read the process environment rather than its argument", err)
	}
}

// A nil getenv is a programming error, and it must not panic inside a
// server startup path.
//
// @decision(DL-047)
func TestResolveRefusesANilGetenv(t *testing.T) {
	if _, err := Resolve(Config{}, nil, nil); err == nil {
		t.Error("Resolve accepted a nil getenv")
	}
}

// A named key variable says where the money comes from, not which
// provider it is for. Without a model there is nothing to ask.
//
// @decision(DL-047)
func TestAConfiguredKeyVariableStillNeedsAModel(t *testing.T) {
	_, err := Resolve(Config{KeyEnv: "SPOV_PREP_KEY"},
		env(map[string]string{"SPOV_PREP_KEY": "k"}), nil)
	if err == nil {
		t.Fatal("Resolve accepted a key variable with no model to ask for")
	}
	if errors.Is(err, ErrNoProvider) {
		t.Error("a misconfiguration reported as ErrNoProvider would be logged as an ordinary state")
	}
}

// A base URL configured beside a detected key redirects the request
// while keeping the ladder's key: the gateway case for a deployment
// that has not separated its budgets yet.
//
// @decision(DL-047)
func TestAConfiguredBaseURLRedirectsADetectedKey(t *testing.T) {
	got, err := Resolve(
		Config{BaseURL: "https://gw.internal/v1", Model: "some-model"},
		env(map[string]string{OpenRouterKeyEnv: "k"}), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.BaseURL != "https://gw.internal/v1" || got.Model != "some-model" {
		t.Errorf("Resolve = %+v, want the configured endpoint and model", got)
	}
	if got.Key != "k" || got.KeyEnv != OpenRouterKeyEnv {
		t.Errorf("Resolve = %+v, want the detected key", got)
	}
}

// Each rung takes the newest model of its family from the list, else
// its literal; a configured model still wins.
//
// @decision(DL-047)
func TestTheLadderPicksTheNewestOfItsFamily(t *testing.T) {
	keys := env(map[string]string{GeminiKeyEnv: "k"})
	newest := func(provider string, family *regexp.Regexp) string {
		if provider == "google" && family.MatchString("gemini-9.1-flash") {
			return "gemini-9.1-flash"
		}
		return ""
	}
	got, err := Resolve(Config{}, keys, newest)
	if err != nil || got.Model != "gemini-9.1-flash" {
		t.Errorf("picked %q (%v)", got.Model, err)
	}
	got, _ = Resolve(Config{}, keys, func(string, *regexp.Regexp) string { return "" })
	if got.Model != GeminiModel {
		t.Errorf("no match picked %q, want the literal", got.Model)
	}
	got, _ = Resolve(Config{Model: "gemini-x"}, keys, newest)
	if got.Model != "gemini-x" {
		t.Errorf("a configured model was overridden: %q", got.Model)
	}
	if again := Reselect(got, newest); again.Model != "gemini-x" {
		t.Errorf("Reselect overrode a configured model: %q", again.Model)
	}
	// A pass picks again from the rung, without the key lookup.
	base, _ := Resolve(Config{}, keys, nil)
	if again := Reselect(base, newest); again.Model != "gemini-9.1-flash" || again.Key != "k" {
		t.Errorf("Reselect = %+v", again)
	}
}
