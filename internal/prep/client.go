package prep

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Timeout bounds the whole call. A job waiting on its preparation is a
// job not doing its work, so this is short and the pass is skipped on
// expiry rather than retried.
//
// @decision(DL-047)
const Timeout = 30 * time.Second

// MaxOutputTokens bounds what the pass may cost. It looks generous for
// a short paragraph and a command because on OpenAI's API the bound
// counts reasoning tokens too, which are not returned; a tighter bound
// truncates before any visible answer is written.
//
// @decision(DL-047)
const MaxOutputTokens = 8000

// Completer is the one thing the pass needs from a model. It is an
// interface rather than the SDK's client so a test never makes a real
// call, and so the SDK stays behind one seam.
//
// @decision(DL-047)
type Completer interface {
	// Complete sends a system and a user message and returns the
	// model's reply.
	Complete(ctx context.Context, system, user string) (string, error)
}

// client is the OpenAI-compatible Completer.
//
// The wire format is the interface here: OpenRouter, Gemini's
// compatibility endpoint, a LiteLLM gateway and OpenAI itself all speak
// it, so one client reaches every provider spov supports without a
// second SDK.
//
// @decision(DL-047)
type client struct {
	api   openai.Client
	model string
	// key is held only so it can be struck from an error message.
	key string
}

// NewClient builds a Completer for a resolved provider.
//
// @decision(DL-047)
func NewClient(sel Selection) (Completer, error) {
	if strings.TrimSpace(sel.Key) == "" {
		return nil, fmt.Errorf("prep: a client needs a key")
	}
	if strings.TrimSpace(sel.Model) == "" {
		return nil, fmt.Errorf("prep: a client needs a model")
	}
	opts := []option.RequestOption{
		option.WithAPIKey(sel.Key),
		// Redirects are refused for the same reason the sandbox
		// requests refuse them: a redirect is the one way this
		// credential could be carried to a host it was not minted for.
		option.WithHTTPClient(&http.Client{
			Timeout:       Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}),
		// The SDK retries by default, which multiplies a slow provider
		// into a job that waits several times the timeout before
		// opening. One attempt, then fall back to the raw goal.
		option.WithMaxRetries(0),
	}
	if sel.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(sel.BaseURL))
	}
	return &client{api: openai.NewClient(opts...), model: sel.Model, key: sel.Key}, nil
}

// Complete asks the model once.
//
// @decision(DL-047)
func (c *client) Complete(ctx context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	resp, err := c.api.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		MaxCompletionTokens: openai.Int(MaxOutputTokens),
	})
	if err != nil {
		// The SDK's error carries the response body verbatim, and a
		// provider that echoes the request back (a 401 quoting the
		// rejected Authorization header) puts the key straight into it,
		// so it is struck here rather than left to internal/server's
		// redact, which is a backstop for patterns it recognises.
		return "", fmt.Errorf("prep: asking %s: %s", c.model, c.scrub(err.Error()))
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("prep: %s answered with no choices", c.model)
	}
	choice := resp.Choices[0]
	// Named rather than left to look like an unparseable answer. A
	// truncated reply is a configuration fact — the bound, or a model
	// that reasons more than it was given room for — and reporting it
	// as "the model's answer was not JSON" sends a reader to the wrong
	// question entirely.
	if content := strings.TrimSpace(choice.Message.Content); content == "" && choice.FinishReason == "length" {
		return "", fmt.Errorf("prep: %s reached the %d-token output bound before it answered",
			c.model, MaxOutputTokens)
	}
	return choice.Message.Content, nil
}

// scrub removes the key from a message. The client is the only thing
// that knows the exact value, so it is the only thing that can.
//
// @decision(DL-047)
func (c *client) scrub(msg string) string {
	if c.key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, c.key, "[redacted]")
}
