package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/store"
)

const testModel = "anthropic/claude-sonnet-5"

// testModelKeys configures the deployment with a key for testModel's
// provider. DL-013 refuses a model the caller holds no key for before
// anything is created, so a test about carrying the model needs one.
func testModelKeys(d *Deps) {
	d.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "sk-fixture"}
	d.Models = FixedModels(testModels())
}

// retiredModel is a model models.dev marks deprecated in testModels.
const retiredModel = "anthropic/claude-retired"

// testModels is a fixed model list holding testModel and retiredModel,
// so these tests do not follow the shipped snapshot.
func testModels() modelcat.List {
	l := modelcat.Fixed(testModel, "anthropic/claude-opus-5", "openai/gpt-6-astra")
	l.Deprecated = map[string][]string{"anthropic": {"claude-retired"}}
	return l
}

// modelFixture and jobModelFixture are the two fixtures with that key.
func modelFixture(t *testing.T) *apiFixture {
	t.Helper()
	return newAPIFixtureWith(t, testModelKeys)
}

func jobModelFixture(t *testing.T) (*apiFixture, *recordingQueue) {
	t.Helper()
	return jobFixtureWith(t, testModelKeys)
}

// TestAJobsModelReachesItsSessionRequest: a job's model lands on the job
// row, its view and its session's request, since the launch reads the
// session and a redelivery reads the job.
//
// @decision(DL-014)
func TestAJobsModelReachesItsSessionRequest(t *testing.T) {
	f, _ := jobModelFixture(t)
	req := jobRequest()
	req.Model = testModel

	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var view JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Model != testModel {
		t.Errorf("the job view lost the model: got %q, want %q", view.Model, testModel)
	}

	job, err := f.store.Jobs().Get(context.Background(), auth.LocalID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Model != testModel {
		t.Errorf("the job row lost the model: got %q, want %q", job.Model, testModel)
	}

	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Model != testModel {
		t.Errorf("the job's session lost the model: got %q, want %q", sess.Request.Model, testModel)
	}
}

// TestAnOmittedModelStaysOmitted: an omitted model stays empty on the job
// and its session, with no default of spov's own substituted.
//
// @decision(DL-014)
func TestAnOmittedModelStaysOmitted(t *testing.T) {
	f, _ := jobFixture(t)
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var view JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Model != "" {
		t.Errorf("an omitted model became %q", view.Model)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Model != "" {
		t.Errorf("an omitted model became %q on the session", sess.Request.Model)
	}
}

// TestAnOversizedModelIsRefusedBeforeAnythingBillable: a model over
// MaxModelBytes is refused on both create paths, which validate
// separately, before anything is queued.
//
// @decision(DL-014)
func TestAnOversizedModelIsRefusedBeforeAnythingBillable(t *testing.T) {
	f, q := jobFixture(t)
	huge := strings.Repeat("m", MaxModelBytes+1)

	req := jobRequest()
	req.Model = huge
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("an oversized model was accepted")
	}
	if !strings.Contains(string(body), "the model is") {
		t.Errorf("the error does not name the model: %s", body)
	}
	if len(q.args) != 0 {
		t.Error("a refused job still reached the queue")
	}

	_, err := f.srv.svc.Create(context.Background(), auth.Principal{
		OrgID: auth.LocalID, UserID: auth.LocalID,
	}, store.SessionRequest{
		Repos: []store.RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
		Model: huge,
	})
	if err == nil {
		t.Fatal("an oversized model was accepted on a session create")
	}
	if !strings.Contains(err.Error(), "the model is") {
		t.Errorf("the session error does not name the model: %v", err)
	}
}

// TestASessionsModelReachesItsView: the create request and the view are
// two mappings with nothing else exercising them — an API client sets
// a model, and a client reads back what the session runs on.
//
// @decision(DL-014)
func TestASessionsModelReachesItsView(t *testing.T) {
	f := modelFixture(t)

	id := createSessionWithModel(t, f, testModel)

	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Model != testModel {
		t.Errorf("the stored request lost the model: got %q, want %q", sess.Request.Model, testModel)
	}
	if got := sessionViewModel(t, f, id); got != testModel {
		t.Errorf("the session view lost the model: got %q, want %q", got, testModel)
	}

	// And through Service.launch into the entrypoint the fake provider
	// was handed. Read through the locked accessor: the launch goroutine
	// appends to Created, so the exported slice races under -race.
	waitFor(t, "the session to be launched", func() bool { return len(f.provider.Specs()) > 0 })
	script := strings.Join(f.provider.Specs()[0].Command, " ")
	if !strings.Contains(script, `"model": "`+testModel+`"`) {
		t.Errorf("the launched sandbox carries no model:\n%s", script)
	}
}

// createSessionWithModel posts a session create and returns its id. The
// create answers with an id alone, so the view is read back separately.
//
// The request body is createSessionWith's, so what these tests send is
// defined once.
func createSessionWithModel(t *testing.T, f *apiFixture, model string) string {
	t.Helper()
	resp, body := createSessionWith(t, f, model)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func sessionViewModel(t *testing.T, f *apiFixture, id string) string {
	t.Helper()
	resp, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %s", resp.StatusCode, body)
	}
	var view SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	return view.Model
}

// TestAPaddedModelIsNormalisedOnBothCreatePaths: surrounding whitespace
// is trimmed on the job and session create paths alike, before DL-013's
// check and before the value reaches the sandbox's config.
//
// @decision(DL-014)
func TestAPaddedModelIsNormalisedOnBothCreatePaths(t *testing.T) {
	f, _ := jobModelFixture(t)

	req := jobRequest()
	req.Model = "  " + testModel + "  "
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var job JobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if job.Model != testModel {
		t.Errorf("a job kept its padding: got %q, want %q", job.Model, testModel)
	}
	jsess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if jsess.Request.Model != testModel {
		t.Errorf("a job's session kept its padding: got %q", jsess.Request.Model)
	}

	id := createSessionWithModel(t, f, "  "+testModel+"  ")
	if got := sessionViewModel(t, f, id); got != testModel {
		t.Errorf("a session kept its padding: got %q, want %q", got, testModel)
	}
}
