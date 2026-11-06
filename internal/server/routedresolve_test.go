package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

func held(name, host, attach string) HeldRoute {
	return HeldRoute{Route: routedcred.Route{Name: name, Hosts: []string{host}, Scheme: "bearer", Attach: attach}.Normal(),
		Secret: "secret-" + name}
}

func names(cs []candidateRoute) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name + ":" + string(c.Tier)
	}
	return out
}

// With no list of its own and no configuration, a launch attaches every
// credential attached always, and never an asked-only one.
//
// @decision(DL-075)
func TestEveryCandidateAttachesByDefault(t *testing.T) {
	cands, _ := candidates([]HeldRoute{held("npm", "npm.acme.com", ""), held("asked", "a.acme.com", "asked")},
		[]HeldRoute{held("maven", "repo.acme.com", "")})
	got, notes, err := attachRoutes(cands, store.RoutedRequest{})
	if err != nil || !slices.Equal(names(got), []string{"npm:user", "maven:org"}) || len(notes) != 0 {
		t.Errorf("attached %v, notes %v, %v", names(got), notes, err)
	}
}

// The configuration's list only narrows: it keeps the named ones of the
// default, never adds an asked-only one or one the caller lacks (noted),
// and [none] keeps nothing.
//
// @decision(DL-075)
func TestAProjectFileCanOnlyNarrowCredentials(t *testing.T) {
	cands, _ := candidates([]HeldRoute{held("npm", "npm.acme.com", ""), held("pypi", "pypi.acme.com", ""),
		held("asked", "a.acme.com", "asked")}, nil)
	got, notes, err := attachRoutes(cands, store.RoutedRequest{Preferred: []string{"pypi", "asked", "someone-elses"}})
	if err != nil || !slices.Equal(names(got), []string{"pypi:user"}) {
		t.Errorf("attached %v, %v", names(got), err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "someone-elses") {
		t.Errorf("notes %v", notes)
	}
	if got, _, _ := attachRoutes(cands, store.RoutedRequest{Preferred: []string{NoCredentials}}); len(got) != 0 {
		t.Errorf("[none] attached %v", names(got))
	}
}

// The launch's own list is exact: it attaches an asked-only credential,
// overrides the configuration, refuses a name the caller lacks, and
// no_credentials attaches none.
//
// @decision(DL-075)
func TestAnAskedCredentialAttachesOnlyByName(t *testing.T) {
	cands, _ := candidates([]HeldRoute{held("npm", "npm.acme.com", ""), held("asked", "a.acme.com", "asked")}, nil)
	got, _, err := attachRoutes(cands, store.RoutedRequest{Names: []string{"asked"}, Preferred: []string{"npm"}})
	if err != nil || !slices.Equal(names(got), []string{"asked:user"}) {
		t.Errorf("attached %v, %v", names(got), err)
	}
	if _, _, err := attachRoutes(cands, store.RoutedRequest{Names: []string{"nope"}}); !errors.Is(err, errRoutedRequest) {
		t.Errorf("an unknown name: %v", err)
	}
	if got, _, _ := attachRoutes(cands, store.RoutedRequest{None: true}); len(got) != 0 {
		t.Errorf("no_credentials attached %v", names(got))
	}
}

// A personal credential takes a host, and a name, from the
// organisation's, which is left out with a note. Two personal ones on a
// host are refused only when both would be attached.
//
// @decision(DL-075)
func TestAPersonalCredentialTakesASharedHost(t *testing.T) {
	cands, notes := candidates([]HeldRoute{held("mine", "npm.acme.com", "")},
		[]HeldRoute{held("shared", "npm.acme.com", ""), held("mine", "other.acme.com", ""), held("maven", "repo.acme.com", "")})
	if !slices.Equal(names(cands), []string{"mine:user", "maven:org"}) || len(notes) != 2 {
		t.Errorf("candidates %v, notes %v", names(cands), notes)
	}
	two, _ := candidates([]HeldRoute{held("a", "npm.acme.com", ""), held("b", "npm.acme.com", "asked")}, nil)
	if got, _, err := attachRoutes(two, store.RoutedRequest{}); err != nil || !slices.Equal(names(got), []string{"a:user"}) {
		t.Errorf("one of two attached: %v, %v", names(got), err)
	}
	if _, _, err := attachRoutes(two, store.RoutedRequest{Names: []string{"a", "b"}}); !errors.Is(err, errRoutedRequest) {
		t.Errorf("two on one host: %v", err)
	}
}

// A needed host no attached credential routes is named; a covered one is
// not.
//
// @decision(DL-075)
func TestANeededHostWithoutACredentialIsNoted(t *testing.T) {
	cands, _ := candidates([]HeldRoute{held("npm", "npm.acme.com", "")}, nil)
	if got := uncoveredNeeds(cands, []string{"npm.acme.com", "repo.acme.com"}); !slices.Equal(got, []string{"repo.acme.com"}) {
		t.Errorf("uncovered %v", got)
	}
	for name, r := range map[string]store.RoutedRequest{
		"both":               {Names: []string{"a"}, None: true},
		"a bad name":         {Names: []string{"Bad_Name"}},
		"none beside others": {Preferred: []string{"none", "a"}},
		"none as a launch":   {Names: []string{"none"}},
		"a wildcard need":    {Needs: []string{"*.acme.com"}},
		"too many needs":     {Needs: slices.Repeat([]string{"a.acme.com"}, MaxNeeds+1)},
	} {
		if checkRoutedRequest(r) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// routedLaunchFixture launches with the proxy and the caller's routed
// credentials sealed in the store.
func routedLaunchFixture(t *testing.T) (*renewFixture, *SealedRoutedCredentials) {
	t.Helper()
	image := func(context.Context) (string, error) { return "spov-proxy-test", nil }
	var src *SealedRoutedCredentials
	api := newAPIFixtureWith(t, func(d *Deps) {
		d.Launch = NewLaunch(&session.Launcher{
			Sandboxes:     d.Provider,
			NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
			HealthTimeout: 5 * time.Second,
			ProxyImage:    image,
		})
		d.ProxyImage = image
		src = &SealedRoutedCredentials{Store: d.Store.Credentials(), Sealer: testSealer(t)}
		d.RoutedCredentials = src
	})
	ctx := context.Background()
	p := auth.Local()
	for _, r := range []routedcred.Route{
		{Name: "acme-npm", Hosts: []string{"npm.acme.com"}, Scheme: "bearer"},
		{Name: "acme-asked", Hosts: []string{"asked.acme.com"}, Scheme: "bearer", Attach: "asked"},
	} {
		if err := src.Put(ctx, p.OrgID, p.UserID, r, "secret-"+r.Name); err != nil {
			t.Fatal(err)
		}
	}
	return &renewFixture{apiFixture: api}, src
}

func sidecarRoutes(t *testing.T, secrets map[string]string) []credproxy.Route {
	t.Helper()
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Routes
}

// A launch reads the caller's routed credentials then, gives the sidecar
// the default set, records it on the session without a secret, and a
// launch list naming one the caller lacks is refused at create.
//
// @decision(DL-075)
func TestALaunchAttachesAndRecordsItsRoutedCredentials(t *testing.T) {
	f, _ := routedLaunchFixture(t)
	f.id = f.create(repoReq("alpha"))
	launch := f.events(f.id)
	routes := sidecarRoutes(t, f.provider.Created[0].Sidecar.Secrets)
	if len(routes) != 1 || routes[0].Name != "acme-npm" || routes[0].Secret != "secret-acme-npm" {
		t.Errorf("the sidecar holds %+v", routes)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.Local().OrgID, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.RoutedCredentials) != 1 || sess.RoutedCredentials[0].Name != "acme-npm" || sess.RoutedCredentials[0].Tier != string(TierUser) {
		t.Errorf("recorded %+v", sess.RoutedCredentials)
	}
	noted := false
	for _, e := range launch {
		noted = noted || strings.Contains(string(e.Data), "routed credentials acme-npm (npm.acme.com)")
	}
	if !noted {
		t.Error("the launch log does not name the routed credentials the proxy holds")
	}
	if b, _ := json.Marshal(sess); strings.Contains(string(b), "secret-acme") {
		t.Errorf("the session row holds a secret: %s", b)
	}
	req := repoReq("beta")
	req.Credentials = []string{"not-mine"}
	if resp, body := f.do(http.MethodPost, "/api/v1/sessions", req); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an unknown name: %d %s", resp.StatusCode, body)
	}
}

// A recreated sidecar holds exactly what the launch recorded, read
// again: a credential removed since is left out and noted, never
// replaced, and an asked-only one the launch did not attach stays out.
//
// @decision(DL-075)
func TestARecreatedSidecarFollowsTheRecordedCredentials(t *testing.T) {
	f, src := routedLaunchFixture(t)
	req := repoReq("alpha")
	req.Credentials = []string{"acme-npm", "acme-asked"}
	f.id = f.create(req)
	f.events(f.id)
	p := auth.Local()
	ctx := context.Background()
	if _, err := src.Delete(ctx, p.OrgID, p.UserID, "acme-asked"); err != nil {
		t.Fatal(err)
	}
	h := f.handle()
	h.KillSidecar()
	f.srv.svc.CheckSidecars(ctx)
	started := h.StartedSidecars()
	if len(started) != 1 {
		t.Fatalf("started %d sidecars", len(started))
	}
	if routes := sidecarRoutes(t, started[0].Secrets); len(routes) != 1 || routes[0].Name != "acme-npm" {
		t.Errorf("the recreated sidecar holds %+v", routes)
	}
	evs, err := f.store.Conversations().ListEvents(ctx, p.OrgID, f.id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	for _, e := range evs {
		noted = noted || (e.Name == MarkerRoutedLeftOut && strings.Contains(e.Text, "acme-asked"))
	}
	if !noted {
		t.Error("the left-out credential was not noted in the session's log")
	}
}
