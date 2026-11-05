package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// modelSandbox answers a conversation create and records what the model
// endpoint was sent, if anything.
type modelSandbox struct {
	srv    *httptest.Server
	paths  []string
	models []string
}

func newModelSandbox(t *testing.T) *modelSandbox {
	t.Helper()
	m := &modelSandbox{}
	m.srv = httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.paths = append(m.paths, r.URL.Path)
		switch {
		case r.URL.Path == "/api/session":
			_, _ = w.Write([]byte(`{"data":{"id":"ses_new"}}`))
		case strings.HasSuffix(r.URL.Path, "/model"):
			raw, _ := io.ReadAll(r.Body)
			// The measured shape: Model.Ref is an object with id and
			// providerID, never the provider/model string (spike S11).
			var body struct {
				Model struct {
					ProviderID string `json:"providerID"`
					ID         string `json:"id"`
					Variant    string `json:"variant"`
				} `json:"model"`
			}
			if err := json.Unmarshal(raw, &body); err != nil || body.Model.ID == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"Expected Model.Ref\n  at [\"model\"]"}`))
				return
			}
			got := body.Model.ProviderID + "/" + body.Model.ID
			if body.Model.Variant != "" {
				got += "#" + body.Model.Variant
			}
			m.models = append(m.models, got)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *modelSandbox) called(suffix string) bool {
	for _, p := range m.paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// jobSessionWithModel seeds a running job session carrying model.
func jobSessionWithModel(t *testing.T, s *Service, model string) store.Session {
	t.Helper()
	job := seedJobSession(t, s, store.StateRunning, "sb-1")
	sess, err := s.deps.Store.Sessions().Get(context.Background(), job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Request.Model = model
	return sess
}

// TestAJobPinsItsConversationToTheModel: a cloned repository's own
// opencode.json can outrank the config file, so a job pins its model
// explicitly rather than relying on it.
//
// @decision(DL-014)
func TestAJobPinsItsConversationToTheModel(t *testing.T) {
	m := newModelSandbox(t)
	s := testService(t, nil)
	sess := jobSessionWithModel(t, s, "anthropic/claude-sonnet-5")

	if _, err := s.jobConversation(context.Background(), Connection{URL: m.srv.URL}, sess); err != nil {
		t.Fatal(err)
	}
	if len(m.models) != 1 {
		t.Fatalf("the model endpoint was called %d times, want 1: %v", len(m.models), m.paths)
	}
	if m.models[0] != "anthropic/claude-sonnet-5" {
		t.Errorf("sent model %q, want anthropic/claude-sonnet-5", m.models[0])
	}
	if !m.called("/ses_new/model") {
		t.Errorf("the model was set on the wrong conversation: %v", m.paths)
	}
}

// TestNoModelSetsNoModel: an empty model means the request is not made
// at all.
//
// @decision(DL-014)
func TestNoModelSetsNoModel(t *testing.T) {
	m := newModelSandbox(t)
	s := testService(t, nil)
	sess := jobSessionWithModel(t, s, "")

	if _, err := s.jobConversation(context.Background(), Connection{URL: m.srv.URL}, sess); err != nil {
		t.Fatal(err)
	}
	if len(m.models) != 0 {
		t.Errorf("an unset model still reached the sandbox: %v", m.models)
	}
}

// TestAFailedModelSetDoesNotLoseTheJob: failing the job over a refused
// set would trade a mismatch that may not happen for a job that
// certainly does not run.
//
// @decision(DL-014)
func TestAFailedModelSetDoesNotLoseTheJob(t *testing.T) {
	var warnings []string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/model") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"ses_new"}}`))
	})))
	defer srv.Close()

	s := testService(t, nil)
	s.deps.Warn = func(msg string) { warnings = append(warnings, msg) }
	sess := jobSessionWithModel(t, s, "anthropic/claude-sonnet-5")

	got, err := s.jobConversation(context.Background(), Connection{URL: srv.URL}, sess)
	if err != nil {
		t.Fatalf("a refused model set lost the job: %v", err)
	}
	if got != "ses_new" {
		t.Errorf("conversation = %q, want the created one", got)
	}
	if len(warnings) == 0 {
		t.Error("a refused model set said nothing; nothing would explain a job on the wrong model")
	}
}

// TestAnExistingConversationIsNotRePinned: jobConversation returns a
// recorded conversation without touching the sandbox, and the model set
// must stay on the creation path, or it becomes a sandbox write per run
// pass rather than per job.
//
// @decision(DL-014) @decision(DL-018)
func TestAnExistingConversationIsNotRePinned(t *testing.T) {
	m := newModelSandbox(t)
	s := testService(t, nil)
	sess := jobSessionWithModel(t, s, "anthropic/claude-sonnet-5")

	if _, err := s.deps.Store.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(s.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: "ses_old",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.jobConversation(context.Background(), Connection{URL: m.srv.URL}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ses_old" {
		t.Errorf("conversation = %q, want the recorded one", got)
	}
	if len(m.paths) != 0 {
		t.Errorf("a recorded conversation still reached the sandbox: %v", m.paths)
	}
}

// TestTheModelIsSentAsAModelRef pins the wire shape against what the
// sandbox actually accepts: a Model.Ref object, not the provider/model
// string the config file takes.
//
// @decision(DL-014)
func TestTheModelIsSentAsAModelRef(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/model") {
			body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"ses_new"}}`))
	})))
	defer srv.Close()

	s := testService(t, nil)
	sess := jobSessionWithModel(t, s, "anthropic/claude-sonnet-5")
	if _, err := s.jobConversation(context.Background(), Connection{URL: srv.URL}, sess); err != nil {
		t.Fatal(err)
	}

	var sent struct {
		Model map[string]any `json:"model"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, body)
	}
	if _, isString := any(sent.Model).(string); isString || sent.Model == nil {
		t.Fatalf("model was not sent as an object: %s", body)
	}
	if sent.Model["providerID"] != "anthropic" || sent.Model["id"] != "claude-sonnet-5" {
		t.Errorf("model = %v, want providerID anthropic and id claude-sonnet-5", sent.Model)
	}
	// modelID is the spelling that looks right and is not the schema's.
	if _, wrong := sent.Model["modelID"]; wrong {
		t.Errorf("the ref carries modelID; the schema field is id: %s", body)
	}
}

// TestAModelThatIsNotAReferenceIsNotSent: the sandbox throws "Invalid
// model reference" on a value with no "/", and spov knows that here —
// so it reports rather than sending something that cannot work.
//
// @decision(DL-014)
func TestAModelThatIsNotAReferenceIsNotSent(t *testing.T) {
	for _, bad := range []string{"claude-sonnet-5", "/claude-sonnet-5", "anthropic/", "anthropic/x#"} {
		t.Run(bad, func(t *testing.T) {
			m := newModelSandbox(t)
			s := testService(t, nil)
			var warnings []string
			s.deps.Warn = func(msg string) { warnings = append(warnings, msg) }
			sess := jobSessionWithModel(t, s, bad)

			if _, err := s.jobConversation(context.Background(), Connection{URL: m.srv.URL}, sess); err != nil {
				t.Fatalf("a malformed model lost the job: %v", err)
			}
			if len(m.models) != 0 {
				t.Errorf("a malformed model was sent anyway: %v", m.models)
			}
			if len(warnings) == 0 {
				t.Error("nothing was reported, so nothing would explain the model being ignored")
			}
		})
	}
}

// TestAVariantSurvivesTheSplit: provider/model#variant is OpenCode's own
// spelling and the ref carries the variant separately.
//
// @decision(DL-014)
func TestAVariantSurvivesTheSplit(t *testing.T) {
	m := newModelSandbox(t)
	s := testService(t, nil)
	sess := jobSessionWithModel(t, s, "anthropic/claude-sonnet-5#thinking")

	if _, err := s.jobConversation(context.Background(), Connection{URL: m.srv.URL}, sess); err != nil {
		t.Fatal(err)
	}
	if len(m.models) != 1 || m.models[0] != "anthropic/claude-sonnet-5#thinking" {
		t.Errorf("sent %v, want the variant carried separately", m.models)
	}
}
