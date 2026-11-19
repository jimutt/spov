package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// modelListFixture is a trimmed 2.3.0 GET /api/model answer, from the
// pinned binary, and fixtureVariants what is kept of it.
const modelListFixture = "testdata/models-2.3.0.json"

var fixtureVariants = map[string][]string{
	"anthropic/claude-fable-5-2":   {"low", "medium", "high", "xhigh", "max"},
	"openai/gpt-5-nano":            {"minimal", "low", "medium", "high"},
	"openrouter/tencent/hy4-flash": {"none", "thinking"},
	"google/gemini-4-neon":         {"low", "medium", "high", "xhigh"},
	"opencode-go/qwen4-max":        {"none", "low", "high", "xhigh"},
}

func readModelListFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(modelListFixture)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The fixture is in the shape the pinned build's document gives a model.
//
// @decision(DL-159)
func TestTheModelListFixtureIsThePinnedBuilds(t *testing.T) {
	doc, err := contract.Load()
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(readModelListFixture(t)), &list); err != nil || len(list.Data) == 0 {
		t.Fatalf("the fixture: %v", err)
	}
	for _, m := range list.Data {
		if err := doc.CheckSchema("Model.Info", m); err != nil {
			t.Errorf("%s", err)
		}
	}
}

// The recorder reads a run's model list once, at its first attach, and
// keeps each model's variants in OpenCode's order with the sandbox they
// came from. A redial reads nothing, nor does another process attaching
// to the same sandbox; a new run's sandbox is read again.
//
// @decision(DL-159)
func TestTheRecorderReadsTheModelListOncePerRun(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.models = readModelListFixture(t)
	ctx := context.Background()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	kept := func() store.ModelVariants {
		got, err := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	waitFor(t, "the model list", func() bool { return kept().SandboxID == "sb-1" })
	if got := kept(); !reflect.DeepEqual(got.Models, fixtureVariants) {
		t.Errorf("kept %v, want %v", got.Models, fixtureVariants)
	}
	f.sb.breakStream()
	waitFor(t, "the second catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s == 2 })
	if n := f.sb.modelReads(); n != 1 {
		t.Errorf("the model list was read %d times across a redial, want once", n)
	}
	f.rec.stopAll()

	// Another process attaches to the same sandbox, then to a new run's.
	again := newRecorderFixture(t)
	again.st, again.sess = f.st, f.sess
	again.rec.store = f.st
	again.sb.models = f.sb.models
	if err := again.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restart's catch-up", func() bool { _, _, s, _ := again.sb.counts(); return s == 1 })
	if n := again.sb.modelReads(); n != 0 {
		t.Errorf("a restart read the model list %d times", n)
	}
	again.rec.stopAll()
	resumed, _ := f.st.Sessions().Get(ctx, "org", f.sess.ID)
	resumed.SandboxID = "sb-2"
	if err := f.st.Sessions().Update(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	next := newRecorderFixture(t)
	next.rec.store = f.st
	next.sb.models = `{"location":{"directory":"/workspace"},"data":[{"id":"m","providerID":"p","variants":[{"id":"high"}]}]}`
	if err := next.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new run's model list", func() bool { return kept().SandboxID == "sb-2" })
	if got := kept(); !reflect.DeepEqual(got.Models, map[string][]string{"p/m": {"high"}}) {
		t.Errorf("the new run kept %v", got.Models)
	}
}

// A model list that cannot be read is said once, writes nothing, and the
// recording goes on; a redial does not try it again.
//
// @decision(DL-159)
func TestAModelListThatFailsIsLoggedOnceAndTheRecordingGoesOn(t *testing.T) {
	for name, set := range map[string]func(*fakeSandbox){
		"a refused read": func(sb *fakeSandbox) { sb.modelsErr = errors.New("503 Service Unavailable") },
		"not a list":     func(sb *fakeSandbox) { sb.models = `{"error":"nope"}` },
		"over the bound": func(sb *fakeSandbox) { sb.models = `{"data":[` + strings.Repeat(" ", MaxModelListBytes) + `]}` },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRecorderFixture(t)
			set(f.sb)
			f.sb.listing = `{"data":[{"id":"ses_1","title":"first"}]}`
			f.sb.messages["ses_1"] = twoMessages
			ctx := context.Background()
			if err := f.rec.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 2 })
			f.sb.breakStream()
			waitFor(t, "the second catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s == 2 })
			said := 0
			for _, w := range f.warnings() {
				if strings.Contains(w, "reasoning modes were not read") {
					said++
				}
			}
			if said != 1 {
				t.Errorf("said %d times, want once: %q", said, f.warnings())
			}
			if got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID); got.SandboxID != "" {
				t.Errorf("a failed read kept %+v", got)
			}
		})
	}
}

// What is kept of a model list: only models with variants, in
// OpenCode's order, without a variant called default, a repeat or an id
// that is not one; a list whose kept part is over its bound keeps
// nothing.
//
// @decision(DL-159)
func TestTheModelListIsReadWithinItsBounds(t *testing.T) {
	got, err := parseModelVariants([]byte(`{"data":[
		{"id":"a","providerID":"p","variants":[{"id":"high"},{"id":"default"},{"id":"low"},{"id":"high"},{"id":"bad id"}]},
		{"id":"a","providerID":"p","variants":[{"id":"other"}]},
		{"id":"none","providerID":"p","variants":[]},
		{"id":"x","providerID":"p/q","variants":[{"id":"high"}]},
		{"id":"","providerID":"p","variants":[{"id":"high"}]}]}`))
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"p/a": {"high", "low"}}) {
		t.Errorf("parse = %v, %v", got, err)
	}
	if got, err := parseModelVariants([]byte(`{"data":[]}`)); err != nil || got == nil || len(got) != 0 {
		t.Errorf("an empty list = %v, %v; want an empty map", got, err)
	}
	var many []string
	for i := range 4000 {
		many = append(many, `{"id":"model-`+strings.Repeat("x", 40)+`-`+strconv.Itoa(i)+`","providerID":"p","variants":[{"id":"low"},{"id":"high"}]}`)
	}
	if _, err := parseModelVariants([]byte(`{"data":[` + strings.Join(many, ",") + `]}`)); err == nil {
		t.Error("a kept list over MaxModelVariantsBytes was accepted")
	}
	for _, body := range []string{`[]`, `{}`, `not json`} {
		if _, err := parseModelVariants([]byte(body)); err == nil {
			t.Errorf("%s was read as a model list", body)
		}
	}
}

// modelReads is how often the model list was read.
func (f *fakeSandbox) modelReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[modelListPath]
}

// The route answers from the store, for the session's current sandbox
// only: nothing read before the recorder kept a list, or when the list
// kept is another sandbox's.
//
// @decision(DL-160)
func TestASessionServesItsCurrentSandboxsModelVariants(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	get := func() ModelVariantsView {
		t.Helper()
		resp, body := f.do(http.MethodGet, "/api/v1/sessions/"+sess.Name+"/model-variants", nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET = %d %s", resp.StatusCode, body)
		}
		var v ModelVariantsView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"models":{`) {
			t.Errorf("the models are not an object: %s", body)
		}
		return v
	}
	if v := get(); v.Read || len(v.Models) != 0 {
		t.Errorf("before the recorder: %+v", v)
	}
	ctx := context.Background()
	if err := f.store.Sessions().SetModelVariants(ctx, auth.LocalID, sess.ID, sess.SandboxID, fixtureVariants); err != nil {
		t.Fatal(err)
	}
	if v := get(); !v.Read || !reflect.DeepEqual(v.Models, fixtureVariants) {
		t.Errorf("after the recorder: %+v", v)
	}
	if err := f.store.Sessions().SetModelVariants(ctx, auth.LocalID, sess.ID, "sb-earlier", fixtureVariants); err != nil {
		t.Fatal(err)
	}
	if v := get(); v.Read || len(v.Models) != 0 {
		t.Errorf("another sandbox's list was served: %+v", v)
	}
}

// A switch takes a #variant only when the session's sandbox listed it
// for that model, and says what it offers otherwise; a reference
// without one is unchanged, and a refusal never reaches the sandbox.
//
// @decision(DL-160)
func TestTheSwitchTakesOnlyAModeTheSandboxOffers(t *testing.T) {
	f := switchFixture(t, store.JobRunning)
	ctx := context.Background()
	refuse := func(model, want string) {
		t.Helper()
		_, err := f.svc.SwitchModel(ctx, f.p, f.sess.ID, ModelSwitch{Model: model})
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", model, err, want)
		}
	}
	refuse("anthropic/claude-opus-5#high",
		"high is not a reasoning mode anthropic/claude-opus-5 offers in this session; this session has not listed its models' reasoning modes yet")
	if err := f.svc.deps.Store.Sessions().SetModelVariants(ctx, f.sess.OrgID, f.sess.ID, f.sess.SandboxID,
		map[string][]string{"anthropic/claude-opus-5": {"low", "high", "max"}}); err != nil {
		t.Fatal(err)
	}
	refuse("anthropic/claude-opus-5#xhigh", "xhigh is not a reasoning mode anthropic/claude-opus-5 offers in this session; it offers low, high, max")
	refuse(testModel+"#high", "it offers none")
	if n := len(f.calls()); n != 0 {
		t.Errorf("a refused mode reached the sandbox: %d requests", n)
	}
	for _, model := range []string{"anthropic/claude-opus-5#max", "anthropic/claude-opus-5"} {
		if _, err := f.svc.SwitchModel(ctx, f.p, f.sess.ID, ModelSwitch{Model: model}); err != nil {
			t.Errorf("%s: %v", model, err)
		}
	}
	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("the sandbox saw %+v", calls)
	}
	if ref, _ := calls[0].body["model"].(map[string]any); ref["variant"] != "max" {
		t.Errorf("sent %v", calls[0].body)
	}
	if ref, _ := calls[1].body["model"].(map[string]any); ref["variant"] != nil {
		t.Errorf("a reference without a mode sent %v", calls[1].body)
	}
	// Another sandbox's list is not this one's.
	if err := f.svc.deps.Store.Sessions().SetModelVariants(ctx, f.sess.OrgID, f.sess.ID, "sb-earlier",
		map[string][]string{"anthropic/claude-opus-5": {"max"}}); err != nil {
		t.Fatal(err)
	}
	refuse("anthropic/claude-opus-5#max", "has not listed")
}
