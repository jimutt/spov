package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// spendBuild uses up the fixture's owner's one new build: the test
// principal is the local user, and the fixture's limit is one.
func spendBuild(t *testing.T, rf *registryFixture) {
	t.Helper()
	v := rf.srv.Service().deps.Variants
	v.MaxNewPerOwner = 1
	other := sandbox.ImageSpec{OpenCodeVersion: variantPin, ExtraPackages: []string{"jq"}}
	if !v.Start(context.Background(), auth.LocalID, other, false) {
		t.Fatal("the first build should start")
	}
	waitFor(t, "the build to finish", func() bool { return len(rf.modal.builds()) == 1 })
	if v.Allow(auth.LocalID) == nil {
		t.Fatal("the window should be spent")
	}
}

// TestACreateThatWouldBuildPastTheLimitIsRefusedBeforeAnythingExists: a
// 429 that names the limit and when it frees, and no session row.
//
// @decision(DL-134)
func TestACreateThatWouldBuildPastTheLimitIsRefusedBeforeAnythingExists(t *testing.T) {
	rf := newRegistryFixture(t, true, newImageFake())
	spendBuild(t, rf)
	req := repoReq("over-the-limit")
	req.Addons = []string{"node@22"}
	res, body := rf.do(http.MethodPost, "/api/v1/sessions", req)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	for _, want := range []string{"1 new image builds in the last 24h0m0s", "the next one is allowed in 24h0m0s"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the refusal %s lacks %q", body, want)
		}
	}
	if n := len(rf.launched()); n != 0 {
		t.Errorf("%d launches started", n)
	}
	_, list := rf.do(http.MethodGet, "/api/v1/sessions", nil)
	if strings.Contains(string(list), "over-the-limit") {
		t.Errorf("a session was recorded: %s", list)
	}
}

// TestACreateThatBootsAnExistingImageIsNeverRefused: a variant that
// exists neither counts nor waits, whoever built it.
//
// @decision(DL-134)
func TestACreateThatBootsAnExistingImageIsNeverRefused(t *testing.T) {
	_, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake(exact.Name()))
	spendBuild(t, rf)
	_ = rf.images.Record(context.Background(), VariantImage(exact))
	if o, _ := rf.launchNode(t); o.Image != exact.Name() || o.Variant != nil {
		t.Fatalf("launched %s, build %v", o.Image, o.Variant)
	}
}

// TestALaunchOnAnOlderBuildIsNotRefusedAndSkipsItsBackgroundBuild: past
// the limit the launch still boots, and the exact build waits for a later
// launch.
//
// @decision(DL-134) @decision(DL-016)
func TestALaunchOnAnOlderBuildIsNotRefusedAndSkipsItsBackgroundBuild(t *testing.T) {
	requested, _ := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake("older"))
	_ = rf.images.Record(context.Background(), store.Image{
		Name: "older", Kind: store.ImageVariant, RequestKey: requested.Name(),
		Versions: []string{"node@22.23.1"}, PublishedAt: rf.now.Add(-48 * time.Hour),
	})
	spendBuild(t, rf)
	if o, _ := rf.launchNode(t); o.Image != "older" || o.Variant != nil {
		t.Fatalf("launched %s, build %v", o.Image, o.Variant)
	}
	time.Sleep(20 * time.Millisecond)
	if b := rf.modal.builds(); len(b) != 1 {
		t.Fatalf("built %v: the background build must not start past the limit", b)
	}
}

// TestABackgroundBuildCountsAgainstTheUserWhoseLaunchCausedIt: the next
// create that needs a build is refused once it has used the window.
//
// @decision(DL-134) @decision(DL-016)
func TestABackgroundBuildCountsAgainstTheUserWhoseLaunchCausedIt(t *testing.T) {
	requested, exact := nodeSpecs(t)
	rf := newRegistryFixture(t, true, newImageFake("older"))
	rf.srv.Service().deps.Variants.MaxNewPerOwner = 1
	_ = rf.images.Record(context.Background(), store.Image{
		Name: "older", Kind: store.ImageVariant, RequestKey: requested.Name(),
		Versions: []string{"node@22.23.1"}, PublishedAt: rf.now.Add(-48 * time.Hour),
	})
	rf.launchNode(t)
	waitFor(t, "the background build to be recorded", func() bool {
		n, err := rf.images.Newest(context.Background(), requested.Name())
		return err == nil && n.Name == exact.Name()
	})
	req := repoReq("needs-a-build")
	req.Apt = []string{"libfoo-dev"}
	if res, body := rf.do(http.MethodPost, "/api/v1/sessions", req); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
}

// TestAJobCreateIsRefusedPastTheBuildLimitToo: the job path asks the
// same question as a session create.
//
// @decision(DL-134)
func TestAJobCreateIsRefusedPastTheBuildLimitToo(t *testing.T) {
	rf := newRegistryFixture(t, true, newImageFake())
	spendBuild(t, rf)
	svc := rf.srv.Service()
	req := store.SessionRequest{Addons: []string{"node@22"}}
	err := svc.refuseBuildOverLimit(context.Background(), auth.LocalID, req)
	if err == nil || !strings.Contains(err.Error(), "the next one is allowed") {
		t.Fatalf("refuseBuildOverLimit = %v", err)
	}
	if toHumaError(err).Error() == "" || !strings.Contains(err.Error(), ErrQuotaExceeded.Error()) {
		t.Errorf("the refusal is a quota error, which the API answers 429: %v", err)
	}
	if err := svc.refuseBuildOverLimit(context.Background(), auth.LocalID, store.SessionRequest{}); err != nil {
		t.Errorf("a request with no add-ons: %v", err)
	}
}

// TestWithoutOnDemandBuildsTheBuildLimitIsNotAsked: nothing builds, so
// nothing is limited; the launch names the operator's command as before.
//
// @decision(DL-134) @decision(DL-016)
func TestWithoutOnDemandBuildsTheBuildLimitIsNotAsked(t *testing.T) {
	rf := newRegistryFixture(t, false, newImageFake())
	spendBuild(t, rf)
	req := store.SessionRequest{Addons: []string{"node@22"}}
	if err := rf.srv.Service().refuseBuildOverLimit(context.Background(), auth.LocalID, req); err != nil {
		t.Errorf("refuseBuildOverLimit = %v", err)
	}
}

// TestATemplatesAddonsCountAgainstTheSameBuildLimit: a template's add-ons
// become the request's before the limit is asked, so a launch from one
// builds its variant as its owner and is refused past their window, as
// a launch that names the add-ons itself is.
//
// @decision(DL-134) @decision(DL-120)
func TestATemplatesAddonsCountAgainstTheSameBuildLimit(t *testing.T) {
	ctx := context.Background()
	rf := newRegistryFixture(t, true, newImageFake())
	templates := &store.FileTemplates{}
	rf.srv.Service().deps.Templates = templates
	for name, settings := range map[string]store.TemplateSettings{
		"node": {Addons: []string{"node@22"}},
		"apt":  {Apt: []string{"libfoo-dev"}},
	} {
		if err := templates.Put(ctx, store.Template{OrgID: auth.LocalID, UserID: auth.LocalID, Name: name,
			TemplateSettings: settings}); err != nil {
			t.Fatal(err)
		}
	}
	req := repoReq("from-node")
	req.Template = "node"
	id := rf.create(req)
	rf.events(id)
	if o := rf.launched(); len(o) != 1 || o[0].Variant == nil || !o[0].BuildVariant || o[0].User != auth.LocalID {
		t.Fatalf("the template's launch is not a build of its variant under its owner: %+v", o)
	}

	spendBuild(t, rf)
	req = repoReq("from-apt")
	req.Template = "apt"
	if res, body := rf.do(http.MethodPost, "/api/v1/sessions", req); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("create from a template past the limit = %d %s", res.StatusCode, body)
	}
	if n := len(rf.launched()); n != 1 {
		t.Errorf("%d launches started, want only the first", n)
	}
}
