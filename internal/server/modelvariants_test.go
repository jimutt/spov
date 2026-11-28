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
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
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

// The recorder reads a run's model list once, beside its first attach,
// and keeps each model's variants in OpenCode's order with the sandbox
// they came from. A redial reads nothing, nor does another process
// attaching to the same sandbox; a new run's sandbox is read again.
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
	again.rec.stopAll()
	if n := again.sb.modelReads(); n != 0 {
		t.Errorf("a restart read the model list %d times", n)
	}
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

// A model list that cannot be read is said once per run of failures,
// writes nothing, and the recording goes on; each redial tries again,
// at most MaxModelListReads times, and a later read that works keeps it.
//
// @decision(DL-159)
func TestAModelListThatFailsIsLoggedOnceAndTriedAgainAtARedial(t *testing.T) {
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
			for n := 1; n <= MaxModelListReads; n++ {
				waitFor(t, "a read of the model list", func() bool { return f.sb.modelReads() == n })
				waitFor(t, "its failure", func() bool { return f.failedReads() == n })
				f.sb.breakStream()
				waitFor(t, "the next catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s == n+1 })
			}
			f.rec.stopAll()
			if n := f.sb.modelReads(); n != MaxModelListReads {
				t.Errorf("read %d times, want %d", n, MaxModelListReads)
			}
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
	t.Run("then a read that works", func(t *testing.T) {
		f := newRecorderFixture(t)
		f.sb.modelsErr = errors.New("503 Service Unavailable")
		ctx := context.Background()
		if err := f.rec.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the failure", func() bool { return f.failedReads() == 1 })
		f.sb.mu.Lock()
		f.sb.modelsErr, f.sb.models = nil, readModelListFixture(t)
		f.sb.mu.Unlock()
		f.sb.breakStream()
		waitFor(t, "the list", func() bool {
			got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID)
			return got.SandboxID == "sb-1"
		})
	})
}

// failedReads is how many of the session's model list reads failed.
func (f *recorderFixture) failedReads() int {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	if st := f.rec.variants[f.sess.ID]; st != nil {
		return st.failures
	}
	return 0
}

// The read is beside the attach: a model list that does not answer
// keeps neither the catch-up nor the live frames waiting, and a session
// at its storage limit, whose capture ends at once, still gets its list.
//
// @decision(DL-159) @decision(DL-017)
func TestTheModelListReadKeepsNothingWaiting(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.models = readModelListFixture(t)
	release := make(chan struct{})
	fetch := f.rec.fetchBounded
	f.rec.fetchBounded = func(ctx context.Context, conn Connection, path string, limit int64) ([]byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fetch(ctx, conn, path, limit)
	}
	f.sb.listing = `{"data":[{"id":"ses_1","title":"first"}]}`
	f.sb.messages["ses_1"] = twoMessages
	ctx := context.Background()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the catch-up while the list hangs", func() bool { return len(f.messages()) == 2 })
	f.sb.send(durableFrameFor(t, "session.step.started", "ses_1", 1, ""))
	waitFor(t, "a live frame while the list hangs", func() bool { return len(f.events()) == 1 })
	close(release)
	waitFor(t, "the list", func() bool {
		got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID)
		return got.SandboxID == "sb-1"
	})

	full := newRecorderFixture(t)
	full.rec.maxRows = 0
	full.sb.models = readModelListFixture(t)
	if err := full.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a full session's list", func() bool {
		got, _ := full.st.Sessions().ModelVariants(ctx, "org", full.sess.ID)
		return got.SandboxID == "sb-1"
	})
}

// What is kept of a model list: only models with variants, in
// OpenCode's order, without a variant called default, a repeat or an id
// that is not one, and at most MaxModelVariants of one model; a list
// whose kept part is over its bound keeps nothing. A hostile list is
// read in time linear in its size.
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
	if got, err := parseModelVariants([]byte(`{"data":[{"id":"none","providerID":"p","variants":[]}]}`)); err != nil || got == nil || len(got) != 0 {
		t.Errorf("a list with no variants = %v, %v; want an empty map", got, err)
	}
	// @decision(DL-183)
	if _, err := parseModelVariants([]byte(`{"data":[]}`)); !errors.Is(err, errNoModelsListed) {
		t.Errorf("a list naming no model = %v; want it not built yet", err)
	}
	var many []string
	for i := range 4000 {
		many = append(many, `{"id":"model-`+strings.Repeat("x", 40)+`-`+strconv.Itoa(i)+`","providerID":"p","variants":[{"id":"low"},{"id":"high"}]}`)
	}
	if _, err := parseModelVariants([]byte(`{"data":[` + strings.Join(many, ",") + `]}`)); err == nil {
		t.Error("a kept list over MaxModelVariantsBytes was accepted")
	}
	// One model with as many distinct variants as fit in the bound.
	var hostile strings.Builder
	hostile.WriteString(`{"data":[{"id":"a","providerID":"p","variants":[`)
	for i := 0; hostile.Len() < MaxModelListBytes-64; i++ {
		if i > 0 {
			hostile.WriteByte(',')
		}
		hostile.WriteString(`{"id":"v` + strconv.Itoa(i) + `"}`)
	}
	hostile.WriteString(`]}]}`)
	start := time.Now()
	capped, err := parseModelVariants([]byte(hostile.String()))
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a hostile list took %s", took)
	}
	if err != nil || len(capped["p/a"]) != MaxModelVariants || capped["p/a"][0] != "v0" {
		t.Errorf("a model with very many variants kept %d, %v; want the first %d", len(capped["p/a"]), err, MaxModelVariants)
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
	moved, _ := f.store.Sessions().Get(ctx, auth.LocalID, sess.ID)
	moved.SandboxID = "sb-later"
	if err := f.store.Sessions().Update(ctx, moved); err != nil {
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
		"high is not a reasoning mode anthropic/claude-opus-5 offers in this session; this session holds no list of its models' reasoning modes")
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
	moved, _ := f.svc.deps.Store.Sessions().Get(ctx, f.sess.OrgID, f.sess.ID)
	moved.SandboxID = "sb-later"
	if err := f.svc.deps.Store.Sessions().Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	refuse("anthropic/claude-opus-5#max", "holds no list")
}

// A sandbox that lists no model has not built its list yet (OpenCode
// from 2.4 does so a few seconds after it starts): the same read asks
// again, a pause apart, and keeps the list once there is one, never an
// empty one; a sandbox that never lists one is a failed read.
//
// @decision(DL-183)
func TestAModelListNotYetBuiltIsAskedForAgain(t *testing.T) {
	const empty = `{"location":{"directory":"/workspace"},"data":[]}`
	t.Run("built a moment later", func(t *testing.T) {
		f := newRecorderFixture(t)
		f.sb.models = readModelListFixture(t)
		reads := 0
		inner := f.rec.fetchBounded
		f.rec.fetchBounded = func(ctx context.Context, conn Connection, path string, limit int64) ([]byte, error) {
			if path == modelListPath {
				if reads++; reads <= 2 {
					return []byte(empty), nil
				}
			}
			return inner(ctx, conn, path, limit)
		}
		ctx := context.Background()
		if err := f.rec.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the model list", func() bool {
			got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID)
			return got.SandboxID == "sb-1"
		})
		if got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID); !reflect.DeepEqual(got.Models, fixtureVariants) {
			t.Errorf("kept %v, want %v", got.Models, fixtureVariants)
		}
		if n := f.failedReads(); n != 0 {
			t.Errorf("%d failed reads, want none", n)
		}
		f.mu.Lock()
		paused := 0
		for _, d := range f.slept {
			if d == ModelListPause {
				paused++
			}
		}
		f.mu.Unlock()
		if paused != 2 {
			t.Errorf("paused %d times between reads, want 2", paused)
		}
	})
	t.Run("never built", func(t *testing.T) {
		f := newRecorderFixture(t)
		f.sb.models = empty
		ctx := context.Background()
		if err := f.rec.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the failure", func() bool { return f.failedReads() == 1 })
		if n := f.sb.modelReads(); n != ModelListWaits+1 {
			t.Errorf("read %d times, want %d", n, ModelListWaits+1)
		}
		if got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID); got.SandboxID != "" {
			t.Errorf("an empty list was kept: %+v", got)
		}
	})
}

// A run's kept list is laid over its org's seen modes when the run ran
// the deployment's pin on the session's sandbox, keeping only the
// providers spov carries; a run on another version or another sandbox
// keeps nothing there.
//
// @decision(DL-186)
func TestARunsModesAreKeptForItsOrgAndVersion(t *testing.T) {
	type run struct{ version, sandbox string }
	for name, r := range map[string]run{
		"a run on the pin":         {"2.4.2", "sb-1"},
		"a run on another version": {"2.3.0", "sb-1"},
		"a run naming no version":  {"", "sb-1"},
		"a run on another sandbox": {"2.4.2", "sb-0"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRecorderFixture(t)
			f.rec.openCode = "2.4.2"
			// A provider spov does not carry is never kept for the org.
			f.sb.models = strings.Replace(readModelListFixture(t), `"data": [`,
				`"data": [{"id":"x","providerID":"acme","variants":[{"id":"high"}]},`, 1)
			if !strings.Contains(f.sb.models, `"acme"`) {
				t.Fatal("the fixture's list was not extended")
			}
			ctx := context.Background()
			if err := f.st.Runs().Insert(ctx, store.Run{OrgID: "org", SessionID: f.sess.ID, OwnerID: "u1", N: 1,
				SandboxID: r.sandbox, OpenCode: r.version, Origin: store.RunFromLaunch, StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := f.rec.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the model list", func() bool {
				got, _ := f.st.Sessions().ModelVariants(ctx, "org", f.sess.ID)
				return got.SandboxID == "sb-1"
			})
			f.rec.stopAll()
			for _, version := range []string{"2.4.2", "2.3.0", ""} {
				seen, err := f.st.ModelModes().Seen(ctx, "org", version)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string][]string{}
				if r == (run{"2.4.2", "sb-1"}) && version == "2.4.2" {
					want = fixtureVariants
				}
				if !reflect.DeepEqual(seen, want) {
					t.Errorf("seen for %q: %v, want %v", version, seen, want)
				}
			}
		})
	}
}

// A launch is offered the newest modes its org's sandboxes reported on
// the pinned build, else the snapshot shipped with that build, and
// nothing from a snapshot of another build.
//
// @decision(DL-186) @decision(DL-185)
func TestALaunchIsOfferedTheSeenModesElseTheSnapshots(t *testing.T) {
	ctx := context.Background()
	snap := modelcat.ModeSnapshot()
	var ref string
	for r := range snap.Models {
		if ref == "" || r < ref {
			ref = r
		}
	}
	st := store.NewMemory()
	svc := &Service{deps: Deps{Store: st, Defaults: Defaults{OpenCodeVersion: snap.OpenCode}}}
	got, err := svc.ModelModes(ctx, "org", ref)
	if err != nil || !reflect.DeepEqual(got, snap.Models[ref]) {
		t.Errorf("with nothing seen %s offers %v, %v; want the snapshot's %v", ref, got, err, snap.Models[ref])
	}
	if err := st.ModelModes().Keep(ctx, "org", snap.OpenCode, map[string][]string{ref: {"only"}, "p/new": {"high"}}); err != nil {
		t.Fatal(err)
	}
	for r, want := range map[string][]string{ref: {"only"}, "p/new": {"high"}, "p/unknown": nil} {
		if got, err := svc.ModelModes(ctx, "org", r); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s offers %v, %v; want %v", r, got, err, want)
		}
	}
	if got, _ := svc.ModelModes(ctx, "another", ref); !reflect.DeepEqual(got, snap.Models[ref]) {
		t.Errorf("another org is offered %v, want the snapshot's", got)
	}
	svc.deps.Defaults.OpenCodeVersion = "9.9.9"
	if got, _ := svc.ModelModes(ctx, "another", ref); got != nil {
		t.Errorf("a deployment on another build is offered the snapshot's %v", got)
	}
}
