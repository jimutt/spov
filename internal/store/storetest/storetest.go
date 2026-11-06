// Package storetest is the contract every Store implementation must
// meet. The memory and Postgres stores both run it. A query the memory store cannot answer is not added to the
// interface.
//
// @decision(DL-003)
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// Factory returns a fresh, empty store for one test.
type Factory func(t *testing.T) store.Store

// Run runs the contract against every store the factory produces.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { tc(t, factory(t)) })
	}
}

var t0 = time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)

func session(id, org, owner, name string, state store.SessionState) store.Session {
	return store.Session{ID: id, OrgID: org, OwnerID: owner, Name: name, State: state,
		Request: store.SessionRequest{Name: name, Repos: []store.RepoRef{{Platform: "github", Owner: "o", Name: "r"}}}}
}

var cases = map[string]func(t *testing.T, s store.Store){
	// @decision(DL-042)
	"an org credential is one row per org and platform": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		oc := s.OrgCredentials()
		if _, err := oc.Get(ctx, "org", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("empty: %v", err)
		}
		put := func(ct, meta string) {
			t.Helper()
			if err := oc.Put(ctx, store.OrgCredential{ID: store.NewID(t0), OrgID: "org", Platform: "github",
				Ciphertext: []byte(ct), Nonce: []byte("n"), KeyVersion: 1, Meta: []byte(meta), UpdatedBy: "admin"}); err != nil {
				t.Fatal(err)
			}
		}
		put("one", `{"app_id": "1"}`)
		first, err := oc.Get(ctx, "org", "github")
		if err != nil || string(first.Ciphertext) != "one" {
			t.Fatalf("got %+v, %v", first, err)
		}
		put("two", `{"app_id": "2"}`)
		second, err := oc.Get(ctx, "org", "github")
		if err != nil || string(second.Ciphertext) != "two" || second.ID != first.ID || !strings.Contains(string(second.Meta), `"2"`) {
			t.Fatalf("replaced: %+v, %v", second, err)
		}
		if _, err := oc.Get(ctx, "other", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org sees it: %v", err)
		}
		// @decision(DL-074)
		if err := oc.Put(ctx, store.OrgCredential{ID: store.NewID(t0), OrgID: "org", Platform: "route:acme",
			Ciphertext: []byte("r"), Nonce: []byte("n"), KeyVersion: 1, Meta: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := oc.Put(ctx, store.OrgCredential{ID: store.NewID(t0), OrgID: "other", Platform: "route:theirs",
			Ciphertext: []byte("r"), Nonce: []byte("n"), KeyVersion: 1, Meta: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if list, err := oc.List(ctx, "org"); err != nil || len(list) != 2 || list[0].Platform != "github" || list[1].Platform != "route:acme" {
			t.Errorf("list = %+v, %v", list, err)
		}
		if err := oc.Delete(ctx, "org", "github"); err != nil {
			t.Fatal(err)
		}
		if err := oc.Delete(ctx, "org", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a second delete: %v", err)
		}
	},
	// @decision(DL-042)
	"admin events list newest first, per org": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for i, org := range []string{"org", "org", "other", "org"} {
			if err := s.AdminEvents().Append(ctx, store.AdminEvent{ID: store.NewID(t0.Add(time.Duration(i) * time.Minute)),
				OrgID: org, Actor: "a", Action: fmt.Sprintf("act%d", i), At: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.AdminEvents().List(ctx, "org", 2)
		if err != nil || len(got) != 2 || got[0].Action != "act3" || got[1].Action != "act1" {
			t.Errorf("listed %+v, %v", got, err)
		}
	},
	// @decision(DL-021)
	"the last event can pass over named markers": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		mark := func(name string) {
			t.Helper()
			if _, err := s.Conversations().PutEvents(ctx, []store.Event{{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, Source: store.SourceSpov, Name: name, At: t0,
			}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Conversations().LastEventExcept(ctx, "org", sess.ID, []string{"spov.b"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("no events: %v", err)
		}
		mark("spov.a")
		mark("spov.b")
		got, err := s.Conversations().LastEventExcept(ctx, "org", sess.ID, []string{"spov.b", "spov.c"})
		if err != nil || got.Name != "spov.a" {
			t.Errorf("LastEventExcept = %q, %v; want the event before the skipped one", got.Name, err)
		}
		if got, err := s.Conversations().LastEventExcept(ctx, "org", sess.ID, nil); err != nil || got.Name != "spov.b" {
			t.Errorf("with nothing skipped = %q, %v", got.Name, err)
		}
		if _, err := s.Conversations().LastEventExcept(ctx, "org", sess.ID, []string{"spov.a", "spov.b"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("everything skipped: %v", err)
		}
	},
	// @decision(DL-017)
	"each conversation's last durable sequence": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		conv := func(oc string) store.Conversation {
			t.Helper()
			c, err := s.Conversations().UpsertConversation(ctx, store.Conversation{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: oc,
			})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		a, b, quiet := conv("ses_a"), conv("ses_b"), conv("ses_quiet")
		put := func(c store.Conversation, source store.EventSource, seq int64) {
			t.Helper()
			if _, err := s.Conversations().PutEvents(ctx, []store.Event{{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: c.ID,
				Source: source, Seq: seq, Name: "session.step.started", At: t0,
			}}); err != nil {
				t.Fatal(err)
			}
		}
		put(a, store.SourceOpenCode, 7)
		put(a, store.SourceOpenCode, 3)
		put(b, store.SourceOpenCode, 2)
		// An ephemeral frame has no place in the durable log.
		put(quiet, store.SourceEphemeral, 0)
		got, err := s.Conversations().LastSeqs(ctx, "org", sess.ID)
		if err != nil || len(got) != 2 || got[a.ID] != 7 || got[b.ID] != 2 {
			t.Errorf("LastSeqs = %v, %v; want %s: 7 and %s: 2 only", got, err, a.ID, b.ID)
		}
		if got, err := s.Conversations().LastSeqs(ctx, "other", sess.ID); err != nil || len(got) != 0 {
			t.Errorf("another org sees %v, %v", got, err)
		}
	},
	// @decision(DL-052)
	"an event is found by its name and a part of its frame": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		put := func(name, raw string) {
			t.Helper()
			if _, err := s.Conversations().PutEvents(ctx, []store.Event{{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, Source: store.SourceSpov,
				Name: name, At: t0, Raw: []byte(raw),
			}}); err != nil {
				t.Fatal(err)
			}
		}
		put("spov.a", `{"id":"msg_1","n":1}`)
		put("spov.b", `{"id":"msg_1"}`)
		put("spov.a", `{"id":"msg_1","n":2}`)
		put("spov.a", `{"id":"msg_2"}`)
		got, err := s.Conversations().FindEvent(ctx, "org", sess.ID, "spov.a", []byte(`"msg_1"`))
		if err != nil || string(got.Raw) != `{"id":"msg_1","n":2}` {
			t.Errorf("FindEvent = %s, %v; want the newest spov.a holding msg_1", got.Raw, err)
		}
		if _, err := s.Conversations().FindEvent(ctx, "org", sess.ID, "spov.b", []byte(`"msg_2"`)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("no match: %v", err)
		}
		if _, err := s.Conversations().FindEvent(ctx, "other", sess.ID, "spov.a", []byte(`"msg_1"`)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org: %v", err)
		}
	},
	// @decision(DL-021) @decision(DL-043)
	"a session's credentials survive and its git token is its own write": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "alpha", store.StateStarting)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		// A copy read before the re-mint, as a slow writer would hold.
		stale, err := s.Sessions().Get(ctx, "org", in.ID)
		if err != nil {
			t.Fatal(err)
		}
		minted, expires := t0.Add(time.Minute), t0.Add(time.Hour)
		if err := s.Sessions().SetGitToken(ctx, "org", in.ID, minted, expires); err != nil {
			t.Fatal(err)
		}
		stale.State = store.StateRunning
		stale.Credentials = []store.SessionCredential{
			{Platform: "github", Kind: store.CredentialRenewed},
			{Platform: "codefloe", Kind: store.CredentialStored},
		}
		// @decision(DL-033)
		stale.Booted = store.BootedImage{Image: "spov-base-x", Addons: []string{"node@22.23.3"}}
		// @decision(DL-075)
		stale.RoutedCredentials = []store.SessionRoutedCredential{{Name: "acme-npm", Tier: "user",
			Hosts: []string{"npm.acme.com"}, Paths: []string{"/npm/"}, Scheme: "bearer"}}
		stale.Request.Routed = store.RoutedRequest{Names: []string{"acme-npm"}, Needs: []string{"npm.acme.com"}}
		if err := s.Sessions().Update(ctx, stale); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().Get(ctx, "org", in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Credentials) != 2 || got.Credentials[1].Kind != store.CredentialStored {
			t.Errorf("credentials = %+v", got.Credentials)
		}
		if got.Booted.Image != "spov-base-x" || len(got.Booted.Addons) != 1 || got.Booted.Addons[0] != "node@22.23.3" {
			t.Errorf("booted = %+v", got.Booted)
		}
		if !reflect.DeepEqual(got.RoutedCredentials, stale.RoutedCredentials) || !reflect.DeepEqual(got.Request.Routed, stale.Request.Routed) {
			t.Errorf("routed = %+v, request %+v", got.RoutedCredentials, got.Request.Routed)
		}
		if got.GitTokenMintedAt == nil || !got.GitTokenMintedAt.Equal(minted) ||
			got.GitTokenExpiresAt == nil || !got.GitTokenExpiresAt.Equal(expires) {
			t.Errorf("an update from an older copy undid the git token: %v, %v", got.GitTokenMintedAt, got.GitTokenExpiresAt)
		}
		if err := s.Sessions().SetGitToken(ctx, "other", in.ID, minted, expires); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org set the git token: %v", err)
		}
		if err := s.Sessions().SetGitToken(ctx, "org", "nope", minted, expires); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a missing session: %v", err)
		}
	},
	// @decision(DL-034)
	// @decision(DL-085)
	"a session's runs: inserted, updated, ended with the session, listed and reopened": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "runs", store.StateRunning)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Runs().Current(ctx, "org", in.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("before any run: %v", err)
		}
		running := t0.Add(time.Minute)
		one := store.Run{OrgID: "org", SessionID: in.ID, OwnerID: "u1", N: 1, SandboxID: "sb-1", Booted: "img",
			OpenCode: "2.0.22", Origin: store.RunFromLaunch, StartedAt: t0, RunningAt: &running, SnapshotState: store.SnapshotPending}
		if err := s.Runs().Insert(ctx, one); err != nil {
			t.Fatal(err)
		}
		if err := s.Runs().Insert(ctx, one); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a second run 1: %v", err)
		}
		if err := s.Runs().Insert(ctx, store.Run{OrgID: "other", SessionID: in.ID, N: 2, Origin: store.RunFromLaunch,
			StartedAt: t0, SnapshotState: store.SnapshotNone}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org's run: %v", err)
		}
		// A pending run still live is not a snapshot to collect.
		if got, _ := s.Runs().Snapshots(ctx, store.SnapshotPending); len(got) != 0 {
			t.Errorf("a live run was offered for collection: %+v", got)
		}
		stopped := t0.Add(time.Hour)
		if ok, err := s.Sessions().MarkStopped(ctx, "org", in.ID, store.StateStopped, stopped, ""); !ok || err != nil {
			t.Fatalf("stop: %v %v", ok, err)
		}
		cur, err := s.Runs().Current(ctx, "org", in.ID)
		if err != nil || cur.EndedAt == nil || !cur.EndedAt.Equal(stopped) || cur.SandboxID != "sb-1" || !cur.Ran() {
			t.Fatalf("the stop did not end the run: %+v, %v", cur, err)
		}
		if got, _ := s.Runs().Snapshots(ctx, store.SnapshotPending); len(got) != 1 || got[0].N != 1 {
			t.Errorf("the ended run is not offered for collection: %+v", got)
		}
		at := stopped.Add(10 * time.Second)
		cur.Snapshot, cur.SnapshotState, cur.SnapshotAt = "im-1", store.SnapshotKept, &at
		cur.OwnerID, cur.StartedAt = "someone else", t0.Add(time.Hour)
		if err := s.Runs().Update(ctx, cur); err != nil {
			t.Fatal(err)
		}
		kept, _ := s.Runs().Current(ctx, "org", in.ID)
		if kept.Snapshot != "im-1" || kept.SnapshotState != store.SnapshotKept || kept.OwnerID != "u1" || !kept.StartedAt.Equal(t0) {
			t.Errorf("update: %+v", kept)
		}
		if err := s.Runs().Update(ctx, store.Run{OrgID: "org", SessionID: in.ID, N: 9, Origin: store.RunFromLaunch,
			SnapshotState: store.SnapshotNone}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a missing run: %v", err)
		}
		two := store.Run{OrgID: "org", SessionID: in.ID, OwnerID: "u1", N: 2, Origin: store.RunFromSnapshot,
			BootedImageID: "im-1", StartedAt: t0.Add(2 * time.Hour), SnapshotState: store.SnapshotFailed}
		if err := s.Runs().Insert(ctx, two); err != nil {
			t.Fatal(err)
		}
		end := t0.Add(3 * time.Hour)
		two.EndedAt = &end
		if err := s.Runs().Update(ctx, two); err != nil {
			t.Fatal(err)
		}
		if err := s.Runs().Reopen(ctx, "org", in.ID); err != nil {
			t.Fatal(err)
		}
		reopened, _ := s.Runs().Current(ctx, "org", in.ID)
		if reopened.N != 2 || reopened.EndedAt != nil || reopened.SnapshotState != store.SnapshotPending {
			t.Errorf("reopen: %+v", reopened)
		}
		other := session(store.NewID(t0.Add(time.Second)), "org", "u1", "norun", store.StateRunning)
		if err := s.Sessions().Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		all, err := s.Runs().List(ctx, "org", []string{in.ID, other.ID})
		if err != nil || len(all) != 1 || len(all[in.ID]) != 2 || all[in.ID][0].N != 1 || all[in.ID][1].N != 2 {
			t.Errorf("list: %+v, %v", all, err)
		}
		if none, _ := s.Runs().List(ctx, "other", []string{in.ID}); len(none) != 0 {
			t.Errorf("another org listed the runs: %+v", none)
		}
		if kept, _ := s.Runs().Snapshots(ctx, store.SnapshotKept); len(kept) != 1 || kept[0].Snapshot != "im-1" {
			t.Errorf("kept snapshots: %+v", kept)
		}
		if got := (store.Run{StartedAt: t0, EndedAt: &stopped}).Stretch(t0.Add(30*time.Minute), t0.Add(5*time.Hour)); got != 30*time.Minute {
			t.Errorf("stretch = %s", got)
		}
	},
	"a session's sealed proxy CA is kept apart and replaced whole": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "gamma", store.StateRunning)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sessions().ProxyCA(ctx, "org", in.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("before any write: %v", err)
		}
		first := store.SealedValue{Ciphertext: []byte("c1"), Nonce: []byte("n1"), KeyVersion: 3}
		if err := s.Sessions().SetProxyCA(ctx, "org", in.ID, first); err != nil {
			t.Fatal(err)
		}
		second := store.SealedValue{Ciphertext: []byte("c2"), Nonce: []byte("n2"), KeyVersion: 4}
		if err := s.Sessions().SetProxyCA(ctx, "org", in.ID, second); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().ProxyCA(ctx, "org", in.ID)
		if err != nil || string(got.Ciphertext) != "c2" || string(got.Nonce) != "n2" || got.KeyVersion != 4 {
			t.Errorf("ProxyCA = %+v, %v; want the second write", got, err)
		}
		if _, err := s.Sessions().ProxyCA(ctx, "other", in.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org read it: %v", err)
		}
		if err := s.Sessions().SetProxyCA(ctx, "other", in.ID, first); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org wrote it: %v", err)
		}
		if err := s.Sessions().SetProxyCA(ctx, "org", "nope", first); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a missing session: %v", err)
		}
		if _, err := s.Sessions().MarkStopped(ctx, "org", in.ID, store.StateStopped, t0, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sessions().ProxyCA(ctx, "org", in.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a stopped session's CA is still kept: %v", err)
		}
	},
	// @decision(DL-015)
	"a session's OpenCode grant generation is its own write": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "beta", store.StateStarting)
		in.OpenCodeGrant = 9 // Create ignores it.
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		stale, err := s.Sessions().Get(ctx, "org", in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stale.OpenCodeGrant != 0 {
			t.Errorf("a new session holds generation %d", stale.OpenCodeGrant)
		}
		if err := s.Sessions().SetOpenCodeGrant(ctx, "org", in.ID, 3); err != nil {
			t.Fatal(err)
		}
		stale.State = store.StateRunning
		if err := s.Sessions().Update(ctx, stale); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Sessions().Get(ctx, "org", in.ID); got.OpenCodeGrant != 3 {
			t.Errorf("an update from an older copy undid the grant generation: %d", got.OpenCodeGrant)
		}
		if err := s.Sessions().SetOpenCodeGrant(ctx, "other", in.ID, 4); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org set the generation: %v", err)
		}
	},
	// @decision(DL-015)
	"a credential lock is taken inside a transaction": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		err := s.Tx(ctx, func(tx store.Store) error {
			return tx.Credentials().Lock(ctx, "org", "u1", "opencode:grant")
		})
		if err != nil {
			t.Fatal(err)
		}
	},
	"ping": func(t *testing.T, s store.Store) {
		if err := s.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	},
	"create then get by id and by name": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "alpha", store.StateStarting)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		byID, err := s.Sessions().Get(ctx, "org", in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if byID.Name != "alpha" || byID.OwnerID != "u1" || byID.Request.Repos[0].Name != "r" {
			t.Errorf("got %+v", byID)
		}
		if byID.CreatedAt.IsZero() || byID.UpdatedAt.IsZero() {
			t.Error("timestamps were not set")
		}
		byName, err := s.Sessions().Get(ctx, "org", "alpha")
		if err != nil || byName.ID != in.ID {
			t.Errorf("by name: %+v, %v", byName, err)
		}
	},
	"get is scoped to the org": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org-a", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sessions().Get(ctx, "org-b", in.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org saw the session by id: %v", err)
		}
		if _, err := s.Sessions().Get(ctx, "org-b", "alpha"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org saw the session by name: %v", err)
		}
	},
	"missing session": func(t *testing.T, s store.Store) {
		if _, err := s.Sessions().Get(context.Background(), "org", "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	},
	"duplicate id conflicts": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		in.Name = "other"
		if err := s.Sessions().Create(ctx, in); !errors.Is(err, store.ErrConflict) {
			t.Errorf("err = %v", err)
		}
	},
	"a live name is unique per org, a stopped one is free": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		first := session(store.NewID(t0), "org", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, first); err != nil {
			t.Fatal(err)
		}
		second := session(store.NewID(t0.Add(time.Second)), "org", "u2", "alpha", store.StateStarting)
		if err := s.Sessions().Create(ctx, second); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a second live session took the same name: %v", err)
		}
		elsewhere := session(store.NewID(t0.Add(time.Second)), "org-2", "u1", "alpha", store.StateRunning)
		if err := s.Sessions().Create(ctx, elsewhere); err != nil {
			t.Fatalf("the same name in another org was refused: %v", err)
		}
		first.State = store.StateStopped
		if err := s.Sessions().Update(ctx, first); err != nil {
			t.Fatal(err)
		}
		if err := s.Sessions().Create(ctx, second); err != nil {
			t.Fatalf("a name freed by a stopped session was refused: %v", err)
		}
		got, err := s.Sessions().Get(ctx, "org", "alpha")
		if err != nil || got.ID != second.ID {
			t.Errorf("by name resolved to %+v, %v; want the live one", got, err)
		}
	},
	"update replaces and refreshes updated_at": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "alpha", store.StateStarting)
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		before, _ := s.Sessions().Get(ctx, "org", in.ID)
		time.Sleep(2 * time.Millisecond)
		in.State = store.StateRunning
		in.SandboxID = "sb-1"
		in.Clones = []string{"o/r"}
		if err := s.Sessions().Update(ctx, in); err != nil {
			t.Fatal(err)
		}
		after, _ := s.Sessions().Get(ctx, "org", in.ID)
		if after.State != store.StateRunning || after.SandboxID != "sb-1" || len(after.Clones) != 1 {
			t.Errorf("got %+v", after)
		}
		if !after.UpdatedAt.After(before.UpdatedAt) {
			t.Error("updated_at did not advance")
		}
		if !after.CreatedAt.Equal(before.CreatedAt) {
			t.Error("created_at changed on update")
		}
		other := in
		other.OrgID = "org-b"
		if err := s.Sessions().Update(ctx, other); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an update from another org was accepted: %v", err)
		}
		taken := session(store.NewID(t0), "org", "u1", "taken", store.StateRunning)
		if err := s.Sessions().Create(ctx, taken); err != nil {
			t.Fatal(err)
		}
		in.Name = "taken"
		if err := s.Sessions().Update(ctx, in); !errors.Is(err, store.ErrConflict) {
			t.Errorf("an update onto a live name was accepted: %v", err)
		}
		in.Name = "alpha"
		missing := session(store.NewID(t0), "org", "u1", "zeta", store.StateRunning)
		if err := s.Sessions().Update(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("update of a missing session: %v", err)
		}
	},
	"list filters by state and owner, newest first": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		a := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		b := session(store.NewID(t0.Add(time.Second)), "org", "u2", "b", store.StateStopped)
		c := session(store.NewID(t0.Add(2*time.Second)), "org", "u1", "c", store.StateFailed)
		d := session(store.NewID(t0.Add(3*time.Second)), "other", "u1", "d", store.StateRunning)
		for _, in := range []store.Session{c, a, d, b} {
			if err := s.Sessions().Create(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		all, err := s.Sessions().List(ctx, "org", store.SessionFilter{})
		if err != nil || len(all) != 3 || all[0].ID != c.ID || all[2].ID != a.ID {
			t.Errorf("all = %v, %v", ids(all), err)
		}
		live, _ := s.Sessions().List(ctx, "org", store.SessionFilter{States: []store.SessionState{store.StateStarting, store.StateRunning}})
		if len(live) != 1 || live[0].ID != a.ID {
			t.Errorf("live = %v", ids(live))
		}
		mine, _ := s.Sessions().List(ctx, "org", store.SessionFilter{OwnerID: "u1"})
		if len(mine) != 2 {
			t.Errorf("u1's = %v", ids(mine))
		}
	},
	// @decision(DL-012)
	"list pages newest first by an id cursor, and counts the whole filter": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		var made []store.Session
		for i := range 5 {
			owner := "u1"
			if i == 2 {
				owner = "u2"
			}
			in := session(store.NewID(t0.Add(time.Duration(i)*time.Second)), "org", owner, fmt.Sprintf("p%d", i), store.StateStopped)
			if err := s.Sessions().Create(ctx, in); err != nil {
				t.Fatal(err)
			}
			made = append(made, in)
		}
		first, err := s.Sessions().List(ctx, "org", store.SessionFilter{OwnerID: "u1", Limit: 2})
		if err != nil || len(first) != 2 || first[0].ID != made[4].ID || first[1].ID != made[3].ID {
			t.Fatalf("first page = %v, %v", ids(first), err)
		}
		next, _ := s.Sessions().List(ctx, "org", store.SessionFilter{OwnerID: "u1", Limit: 2, Before: first[1].ID})
		if len(next) != 2 || next[0].ID != made[1].ID || next[1].ID != made[0].ID {
			t.Errorf("second page = %v, want %s %s (skipping u2's)", ids(next), made[1].ID, made[0].ID)
		}
		last, _ := s.Sessions().List(ctx, "org", store.SessionFilter{OwnerID: "u1", Limit: 2, Before: made[0].ID})
		if len(last) != 0 {
			t.Errorf("past the oldest = %v", ids(last))
		}
		n, err := s.Sessions().Count(ctx, "org", store.SessionFilter{OwnerID: "u1", Limit: 1, Before: made[1].ID})
		if err != nil || n != 4 {
			t.Errorf("count = %d, %v; want 4, ignoring the page", n, err)
		}
	},
	"by sandbox crosses orgs": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		in.SandboxID = "sb-42"
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().BySandbox(ctx, "sb-42")
		if err != nil || got.ID != in.ID {
			t.Errorf("got %+v, %v", got, err)
		}
		if _, err := s.Sessions().BySandbox(ctx, "sb-x"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown sandbox: %v", err)
		}
		if _, err := s.Sessions().BySandbox(ctx, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an empty sandbox id matched something: %v", err)
		}
	},
	"returned sessions are copies": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		in := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		in.Clones = []string{"x"}
		in.Booted.Addons = []string{"node@22.23.3"}
		in.Request.Sync = &store.SessionSync{Branch: "main", BaseCommit: strings.Repeat("a", 40)}
		if err := s.Sessions().Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Sessions().Get(ctx, "org", in.ID)
		got.Clones[0] = "mutated"
		got.Request.Repos[0].Name = "mutated"
		got.Booted.Addons[0] = "mutated"
		got.Request.Sync.Branch = "mutated"
		again, _ := s.Sessions().Get(ctx, "org", in.ID)
		if again.Clones[0] != "x" || again.Request.Repos[0].Name != "r" || again.Booted.Addons[0] != "node@22.23.3" || again.Request.Sync.Branch != "main" {
			t.Error("a caller's mutation reached the store")
		}
	},
	// The concurrency quota counts these, inside the create transaction.
	//
	// @decision(DL-009)
	"live sessions are counted per owner": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for _, tc := range []struct {
			owner string
			state store.SessionState
		}{
			{"u1", store.StateStarting},
			{"u1", store.StateRunning},
			{"u1", store.StateStopped},
			{"u1", store.StateFailed},
			{"u2", store.StateRunning},
		} {
			sess := session(store.NewID(t0), "org", tc.owner, store.NewID(t0), tc.state)
			if err := s.Sessions().Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
		}
		n, err := s.Sessions().CountLive(ctx, "org", "u1")
		if err != nil {
			t.Fatal(err)
		}
		// Starting and running only: a stopped session frees the slot.
		if n != 2 {
			t.Errorf("CountLive = %d, want 2", n)
		}
		if n, err = s.Sessions().CountLive(ctx, "org", "u2"); err != nil || n != 1 {
			t.Errorf("CountLive for u2 = %d, %v", n, err)
		}
		// Another org's sessions are not this one's.
		if n, err = s.Sessions().CountLive(ctx, "other", "u1"); err != nil || n != 0 {
			t.Errorf("CountLive across orgs = %d, %v", n, err)
		}
		if n, err = s.Sessions().CountLive(ctx, "org", "nobody"); err != nil || n != 0 {
			t.Errorf("CountLive for an unknown owner = %d, %v", n, err)
		}
	},
	// A quota override survives a round trip, including its duration.
	//
	// @decision(DL-009)
	"a user quota is stored and read back": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		u := store.User{ID: store.NewID(t0), OrgID: "org", Email: "q@example.com"}
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Quota.Empty() {
			t.Errorf("a new user has a quota: %+v", got.Quota)
		}

		max := 7
		timeout := 90 * time.Minute
		// @decision(DL-049)
		clock := 45 * time.Minute
		got.Quota = store.Quota{MaxConcurrentSessions: &max, MaxTimeout: &timeout, MaxJobDuration: &clock}
		if err := s.Users().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		again, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if again.Quota.MaxConcurrentSessions == nil || *again.Quota.MaxConcurrentSessions != 7 {
			t.Errorf("MaxConcurrentSessions = %v", again.Quota.MaxConcurrentSessions)
		}
		if again.Quota.MaxTimeout == nil || *again.Quota.MaxTimeout != 90*time.Minute {
			t.Errorf("MaxTimeout = %v", again.Quota.MaxTimeout)
		}
		if again.Quota.MaxJobDuration == nil || *again.Quota.MaxJobDuration != 45*time.Minute {
			t.Errorf("MaxJobDuration = %v", again.Quota.MaxJobDuration)
		}
		// Fields the override does not name stay unset, so raising one
		// limit does not pin the others.
		if again.Quota.MaxCPU != nil {
			t.Errorf("MaxCPU was pinned: %v", *again.Quota.MaxCPU)
		}
		// Clearing it works too.
		again.Quota = store.Quota{}
		if err := s.Users().Update(ctx, again); err != nil {
			t.Fatal(err)
		}
		cleared, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !cleared.Quota.Empty() {
			t.Errorf("the quota was not cleared: %+v", cleared.Quota)
		}
	},
	// A default model survives both writes, not just the insert.
	// UpdateUser is a hand-written query feeding sqlc, so a column added
	// to the table and to Create — and forgotten in Update — stores a
	// setting that silently reverts on the next save.
	//
	// @decision(DL-014)
	"a user default model is stored and read back": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		u := store.User{ID: store.NewID(t0), OrgID: "org", Email: "m@example.com",
			GitName: "Ada", GitEmail: "ada@example.com",
			DefaultModel: "anthropic/claude-sonnet-5"}
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DefaultModel != u.DefaultModel {
			t.Fatalf("the insert lost the default model: %q", got.DefaultModel)
		}

		got.DefaultModel = "openai/gpt-6-astra"
		if err := s.Users().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		again, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if again.DefaultModel != "openai/gpt-6-astra" {
			t.Errorf("the update did not store the default model: %q", again.DefaultModel)
		}
		// The git identity rides in the same statement and must not be
		// dropped by a write that was only about the model.
		if again.GitEmail != u.GitEmail {
			t.Errorf("the update dropped the git identity: %+v", again)
		}

		// Clearing is how a user goes back to OpenCode's own default, so
		// an empty value has to be storable rather than ignored.
		again.DefaultModel = ""
		if err := s.Users().Update(ctx, again); err != nil {
			t.Fatal(err)
		}
		cleared, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cleared.DefaultModel != "" {
			t.Errorf("the default model was not cleared: %q", cleared.DefaultModel)
		}
	},
	// Both git access defaults go through Create and Update, and one
	// is not stored in the other's place.
	//
	// @decision(DL-043)
	"a user's git access defaults are stored and read back": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		u := store.User{ID: store.NewID(t0), OrgID: "org", Email: "g@example.com",
			DefaultGitAccess: store.GitAccessAll}
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DefaultGitAccess != store.GitAccessAll || got.DefaultJobGitAccess != "" {
			t.Fatalf("the insert stored %q and %q", got.DefaultGitAccess, got.DefaultJobGitAccess)
		}
		got.DefaultGitAccess, got.DefaultJobGitAccess = "", store.GitAccessRepos
		if err := s.Users().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		again, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if again.DefaultGitAccess != "" || again.DefaultJobGitAccess != store.GitAccessRepos {
			t.Errorf("the update stored %q and %q", again.DefaultGitAccess, again.DefaultJobGitAccess)
		}
	},
	// A user's default job duration survives the insert and the update.
	//
	// @decision(DL-049)
	"a user's default job duration is stored and read back": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		u := store.User{ID: store.NewID(t0), OrgID: "org", Email: "d@example.com", DefaultJobDuration: 2 * time.Hour}
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil || got.DefaultJobDuration != 2*time.Hour {
			t.Fatalf("the insert stored %s, %v; want 2h", got.DefaultJobDuration, err)
		}
		got.DefaultJobDuration = 0
		if err := s.Users().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		if again, err := s.Users().Get(ctx, "org", u.ID); err != nil || again.DefaultJobDuration != 0 {
			t.Errorf("the update stored %s, %v; want it cleared", again.DefaultJobDuration, err)
		}
	},
	// The durable cancel signal: a launch runs on one replica but may be
	// deleted from any, and the pre-sandbox window is exactly when there
	// is no sandbox id to terminate through.
	//
	// @decision(DL-003)
	"a cancel is recorded and read back": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "a", store.StateStarting)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().CancelRequested(ctx, "org", sess.ID)
		if err != nil || got {
			t.Fatalf("CancelRequested before any = %v, %v", got, err)
		}
		ok, err := s.Sessions().RequestCancel(ctx, "org", sess.ID, t0)
		if err != nil || !ok {
			t.Fatalf("RequestCancel = %v, %v", ok, err)
		}
		if got, err = s.Sessions().CancelRequested(ctx, "org", sess.ID); err != nil || !got {
			t.Fatalf("CancelRequested after = %v, %v", got, err)
		}
		// It survives the launch writing the row, which is the whole
		// point: the launch reads a session, then updates it, and must
		// not carry a stale flag back over the delete's.
		sess.State = store.StateRunning
		sess.SandboxID = "sb-1"
		if err := s.Sessions().Update(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if got, err = s.Sessions().CancelRequested(ctx, "org", sess.ID); err != nil || !got {
			t.Errorf("an Update cleared the cancel flag: %v, %v", got, err)
		}
	},
	// A launch that committed is past cancelling: the delete must take
	// the terminate path instead, and RequestCancel says so by
	// reporting that it changed nothing.
	//
	// @decision(DL-003)
	"a cancel is refused once the session is running": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		ok, err := s.Sessions().RequestCancel(ctx, "org", sess.ID, t0)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("a running session accepted a cancel")
		}
		if got, err := s.Sessions().CancelRequested(ctx, "org", sess.ID); err != nil || got {
			t.Errorf("CancelRequested = %v, %v", got, err)
		}
	},
	// Cancelling is scoped to an org like every other session query.
	//
	// @decision(DL-003)
	"a cancel is scoped to its org": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "a", store.StateStarting)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		ok, err := s.Sessions().RequestCancel(ctx, "other", sess.ID, t0)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("another org cancelled this session")
		}
		if got, _ := s.Sessions().CancelRequested(ctx, "org", sess.ID); got {
			t.Error("another org's cancel was recorded")
		}
	},
	"mints are recorded, listed and counted per session": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			m := store.Mint{ID: store.NewID(t0.Add(time.Duration(i) * time.Minute)), OrgID: "org", SessionID: sess.ID,
				UserID: "u1", Port: 4096, CreatedAt: t0.Add(time.Duration(i) * time.Minute), ExpiresAt: t0.Add(24 * time.Hour)}
			if err := s.Mints().Create(ctx, m); err != nil {
				t.Fatal(err)
			}
			if err := s.Mints().Create(ctx, m); !errors.Is(err, store.ErrConflict) {
				t.Errorf("duplicate mint id: %v", err)
			}
		}
		// A mint against a different session, to prove the listing and
		// the count are per session. The session must exist: a store
		// with referential integrity refuses a mint that points at
		// nothing, and that is the behaviour worth having.
		elsewhere := session(store.NewID(t0.Add(time.Hour)), "org", "u1", "elsewhere", store.StateRunning)
		if err := s.Sessions().Create(ctx, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := s.Mints().Create(ctx, store.Mint{ID: store.NewID(t0), OrgID: "org", SessionID: elsewhere.ID, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if err := s.Mints().Create(ctx, store.Mint{ID: store.NewID(t0), OrgID: "org-z", SessionID: sess.ID, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Mints().ListBySession(ctx, "org", sess.ID)
		if err != nil || len(got) != 3 || got[0].CreatedAt.After(got[1].CreatedAt) {
			t.Errorf("list = %+v, %v", got, err)
		}
		if n, _ := s.Mints().CountSince(ctx, "org", sess.ID, t0.Add(time.Minute)); n != 2 {
			t.Errorf("count since t0+1m = %d, want 2", n)
		}
		if n, _ := s.Mints().CountSince(ctx, "other-org", sess.ID, t0); n != 0 {
			t.Errorf("another org counted %d mints", n)
		}
	},
	// @decision(DL-065)
	"lock finds a session by id and org only": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := session(store.NewID(t0), "org", "u1", "locked", store.StateRunning)
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		err := s.Tx(ctx, func(tx store.Store) error { return tx.Sessions().Lock(ctx, "org", sess.ID) })
		if err != nil {
			t.Errorf("lock: %v", err)
		}
		for _, c := range []struct{ org, id string }{{"other-org", sess.ID}, {"org", "locked"}, {"org", "nope"}} {
			if err := s.Sessions().Lock(ctx, c.org, c.id); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("lock %s/%s: %v, want not found", c.org, c.id, err)
			}
		}
	},
	"list live crosses orgs and skips the rest": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for i, in := range []store.Session{
			session(store.NewID(t0), "org-a", "u1", "a", store.StateRunning),
			session(store.NewID(t0.Add(time.Second)), "org-b", "u1", "b", store.StateStarting),
			session(store.NewID(t0.Add(2*time.Second)), "org-a", "u1", "c", store.StateStopped),
		} {
			if err := s.Sessions().Create(ctx, in); err != nil {
				t.Fatalf("%d: %v", i, err)
			}
		}
		live, err := s.Sessions().ListLive(ctx)
		if err != nil || len(live) != 2 || live[0].OrgID != "org-a" || live[1].OrgID != "org-b" {
			t.Errorf("live = %v, %v", ids(live), err)
		}
	},
	"users are created, found by id and by email, and scoped to the org": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		u := store.User{ID: store.NewID(t0), OrgID: "org", Email: "A@Example.com", Name: "Ada", GithubLogin: "ada", Admin: true}
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := s.Users().Get(ctx, "org", u.ID)
		if err != nil || got.Name != "Ada" || !got.Admin {
			t.Fatalf("got %+v, %v", got, err)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Error("timestamps were not set")
		}
		// E-mail is an identity, so it matches regardless of case.
		byEmail, err := s.Users().ByEmail(ctx, "org", "a@example.com")
		if err != nil || byEmail.ID != u.ID {
			t.Errorf("by email: %+v, %v", byEmail, err)
		}
		if _, err := s.Users().Get(ctx, "other", u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org saw the user: %v", err)
		}
		if _, err := s.Users().ByEmail(ctx, "other", "a@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org saw the user by email: %v", err)
		}
		dup := store.User{ID: store.NewID(t0.Add(time.Second)), OrgID: "org", Email: "a@EXAMPLE.com"}
		if err := s.Users().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a duplicate e-mail was accepted: %v", err)
		}
	},
	"users are updated, listed and deleted": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		u := store.User{ID: store.NewID(t0), OrgID: "org", Email: "a@example.com", Name: "Ada"}
		other := store.User{ID: store.NewID(t0.Add(time.Second)), OrgID: "org", Email: "b@example.com", Name: "Bob"}
		for _, in := range []store.User{u, other} {
			if err := s.Users().Create(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		login := t0.Add(time.Hour)
		u.Name = "Ada L"
		u.LastLoginAt = &login
		if err := s.Users().Update(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Users().Get(ctx, "org", u.ID)
		if got.Name != "Ada L" || got.LastLoginAt == nil || !got.LastLoginAt.Equal(login) {
			t.Errorf("got %+v", got)
		}
		u.Email = "b@example.com"
		if err := s.Users().Update(ctx, u); !errors.Is(err, store.ErrConflict) {
			t.Errorf("an update onto another user's e-mail was accepted: %v", err)
		}
		u.Email = "a@example.com"
		list, err := s.Users().List(ctx, "org")
		if err != nil || len(list) != 2 || list[0].ID != u.ID {
			t.Errorf("list = %d users, %v", len(list), err)
		}
		if err := s.Users().Delete(ctx, "other", u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org deleted the user: %v", err)
		}
		if err := s.Users().Delete(ctx, "org", u.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Users().Get(ctx, "org", u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a deleted user is still there: %v", err)
		}
	},
	"tokens are found by hash across orgs, touched and revoked": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		exp := t0.Add(90 * 24 * time.Hour)
		tok := store.APIToken{ID: store.NewID(t0), OrgID: "org", UserID: "u1",
			Hash: []byte("hash-a"), Name: "laptop", ExpiresAt: &exp}
		if err := s.Tokens().Create(ctx, tok); err != nil {
			t.Fatal(err)
		}
		// The authenticator has only the bearer token, so the lookup
		// names no org.
		got, err := s.Tokens().ByHash(ctx, []byte("hash-a"))
		if err != nil || got.ID != tok.ID || got.Name != "laptop" {
			t.Fatalf("by hash: %+v, %v", got, err)
		}
		if !got.Usable(t0) {
			t.Error("a fresh token is not usable")
		}
		if got.Usable(exp.Add(time.Second)) {
			t.Error("an expired token is usable")
		}
		if _, err := s.Tokens().ByHash(ctx, []byte("nope")); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown hash: %v", err)
		}
		if _, err := s.Tokens().ByHash(ctx, nil); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an empty hash matched: %v", err)
		}
		dup := store.APIToken{ID: store.NewID(t0.Add(time.Second)), OrgID: "org", UserID: "u1", Hash: []byte("hash-a")}
		if err := s.Tokens().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a duplicate hash was accepted: %v", err)
		}
		used := t0.Add(time.Minute)
		if err := s.Tokens().Touch(ctx, "org", tok.ID, used); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Tokens().ByHash(ctx, []byte("hash-a")); got.LastUsedAt == nil || !got.LastUsedAt.Equal(used) {
			t.Errorf("last used = %v", got.LastUsedAt)
		}
		// ByID is the revoke endpoint's lookup, and it is scoped to an
		// org: a token id from one org must not resolve in another.
		//
		// @decision(DL-006)
		byID, err := s.Tokens().ByID(ctx, "org", tok.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if byID.ID != tok.ID || byID.UserID != tok.UserID {
			t.Errorf("by id = %+v", byID)
		}
		if _, err := s.Tokens().ByID(ctx, "other", tok.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org resolved the token: %v", err)
		}
		if _, err := s.Tokens().ByID(ctx, "org", "no-such-token"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an unknown id resolved: %v", err)
		}

		mine, err := s.Tokens().ListByUser(ctx, "org", "u1")
		if err != nil || len(mine) != 1 {
			t.Errorf("list = %d, %v", len(mine), err)
		}
		if n, _ := s.Tokens().ListByUser(ctx, "org", "u2"); len(n) != 0 {
			t.Errorf("another user saw %d tokens", len(n))
		}
		rev := t0.Add(2 * time.Hour)
		if err := s.Tokens().Revoke(ctx, "org", tok.ID, rev); err != nil {
			t.Fatal(err)
		}
		after, _ := s.Tokens().ByHash(ctx, []byte("hash-a"))
		if after.RevokedAt == nil || after.Usable(t0) {
			t.Error("a revoked token is still usable")
		}
		if err := s.Tokens().Revoke(ctx, "other", tok.ID, rev); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org revoked the token: %v", err)
		}
	},
	"a device code's org follows the approving user": func(t *testing.T, s store.Store) {
		// A code is created in one org and takes the approving user's
		// on approval, so both stores must persist the change. An update
		// that omitted org_id would diverge from the memory store, and with
		// one org the divergence is invisible.
		//
		// @decision(DL-006)
		ctx := context.Background()
		d := store.DeviceCode{
			ID: store.NewID(t0), OrgID: "org", Hash: []byte("dc-org"), UserCode: "BCDF-ORGX",
			Status: store.DevicePending, ExpiresAt: t0.Add(10 * time.Minute), CreatedAt: t0,
		}
		if err := s.DeviceCodes().Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		d.OrgID = "org-2"
		d.UserID = "approver"
		d.Status = store.DeviceApproved
		if err := s.DeviceCodes().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
		got, err := s.DeviceCodes().ByHash(ctx, []byte("dc-org"))
		if err != nil {
			t.Fatal(err)
		}
		if got.OrgID != "org-2" {
			t.Errorf("org = %q after approval, want org-2", got.OrgID)
		}
		if got.UserID != "approver" || got.Status != store.DeviceApproved {
			t.Errorf("device code = %+v", got)
		}
	},
	"device codes are found by hash and user code, updated and reaped": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		d := store.DeviceCode{ID: store.NewID(t0), OrgID: "org", Hash: []byte("dc-a"), UserCode: "BCDF-GHJK",
			Status: store.DevicePending, Name: "laptop", ExpiresAt: t0.Add(10 * time.Minute), IntervalSeconds: 5}
		if err := s.DeviceCodes().Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		got, err := s.DeviceCodes().ByHash(ctx, []byte("dc-a"))
		if err != nil || got.UserCode != "BCDF-GHJK" || got.Status != store.DevicePending {
			t.Fatalf("by hash: %+v, %v", got, err)
		}
		byUser, err := s.DeviceCodes().ByUserCode(ctx, "BCDF-GHJK")
		if err != nil || byUser.ID != d.ID {
			t.Fatalf("by user code: %+v, %v", byUser, err)
		}
		if _, err := s.DeviceCodes().ByUserCode(ctx, "ZZZZ-ZZZZ"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user code: %v", err)
		}
		dup := store.DeviceCode{ID: store.NewID(t0.Add(time.Second)), OrgID: "org",
			Hash: []byte("dc-b"), UserCode: "BCDF-GHJK", ExpiresAt: t0.Add(time.Minute)}
		if err := s.DeviceCodes().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a duplicate user code was accepted: %v", err)
		}
		polled := t0.Add(time.Second)
		got.Status = store.DeviceApproved
		got.UserID = "u1"
		got.LastPolledAt = &polled
		got.IntervalSeconds = 10
		if err := s.DeviceCodes().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		after, _ := s.DeviceCodes().ByHash(ctx, []byte("dc-a"))
		if after.Status != store.DeviceApproved || after.UserID != "u1" || after.IntervalSeconds != 10 {
			t.Errorf("after update: %+v", after)
		}
		if after.LastPolledAt == nil || !after.LastPolledAt.Equal(polled) {
			t.Errorf("last polled = %v", after.LastPolledAt)
		}
		expired := store.DeviceCode{ID: store.NewID(t0), OrgID: "org", Hash: []byte("dc-old"),
			UserCode: "LMNP-QRST", Status: store.DevicePending, ExpiresAt: t0.Add(-time.Minute)}
		if err := s.DeviceCodes().Create(ctx, expired); err != nil {
			t.Fatal(err)
		}
		n, err := s.DeviceCodes().DeleteExpired(ctx, t0)
		if err != nil || n != 1 {
			t.Errorf("reaped %d, %v; want 1", n, err)
		}
		if _, err := s.DeviceCodes().ByHash(ctx, []byte("dc-old")); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an expired code survived: %v", err)
		}
		if err := s.DeviceCodes().Delete(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.DeviceCodes().Delete(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a second delete: %v", err)
		}
	},
	"credentials are one per user and platform, and overwritten in place": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		c := store.UserCredential{ID: store.NewID(t0), OrgID: "org", UserID: "u1", Platform: "github",
			Ciphertext: []byte("sealed"), Nonce: []byte("nonce"), KeyVersion: 1, VerifiedAs: "ada"}
		if err := s.Credentials().Put(ctx, c); err != nil {
			t.Fatal(err)
		}
		got, err := s.Credentials().Get(ctx, "org", "u1", "github")
		if err != nil || string(got.Ciphertext) != "sealed" || got.KeyVersion != 1 || got.VerifiedAs != "ada" {
			t.Fatalf("got %+v, %v", got, err)
		}
		// A second Put for the same platform replaces rather than
		// duplicating, which is what makes rotation a Put.
		c2 := c
		c2.ID = store.NewID(t0.Add(time.Second))
		c2.Ciphertext = []byte("resealed")
		c2.KeyVersion = 2
		if err := s.Credentials().Put(ctx, c2); err != nil {
			t.Fatal(err)
		}
		after, _ := s.Credentials().Get(ctx, "org", "u1", "github")
		if string(after.Ciphertext) != "resealed" || after.KeyVersion != 2 {
			t.Errorf("after rotation: %+v", after)
		}
		// What the forge said about the token is kept, and replaced with
		// the token. @decision(DL-041)
		if after.Scopes != nil || !after.ExpiresAt.IsZero() {
			t.Errorf("facts appeared from nowhere: %+v", after)
		}
		c3 := c2
		c3.Scopes = []string{"api", "read_user"}
		c3.ExpiresAt = t0.Add(30 * 24 * time.Hour)
		if err := s.Credentials().Put(ctx, c3); err != nil {
			t.Fatal(err)
		}
		facts, _ := s.Credentials().Get(ctx, "org", "u1", "github")
		if !slices.Equal(facts.Scopes, c3.Scopes) || !facts.ExpiresAt.Equal(c3.ExpiresAt) {
			t.Errorf("facts = %v %v", facts.Scopes, facts.ExpiresAt)
		}
		c4 := c3
		c4.Scopes = []string{}
		if err := s.Credentials().Put(ctx, c4); err != nil {
			t.Fatal(err)
		}
		if empty, _ := s.Credentials().Get(ctx, "org", "u1", "github"); empty.Scopes != nil {
			t.Errorf("empty scopes read back as %#v, want nil", empty.Scopes)
		}
		list, _ := s.Credentials().ListByUser(ctx, "org", "u1")
		if len(list) != 1 {
			t.Errorf("a rotated credential was duplicated: %d rows", len(list))
		}
		forgejo := store.UserCredential{ID: store.NewID(t0), OrgID: "org", UserID: "u1", Platform: "forgejo",
			Ciphertext: []byte("x"), Nonce: []byte("n"), KeyVersion: 1}
		if err := s.Credentials().Put(ctx, forgejo); err != nil {
			t.Fatal(err)
		}
		list, _ = s.Credentials().ListByUser(ctx, "org", "u1")
		if len(list) != 2 || list[0].Platform != "forgejo" {
			t.Errorf("list = %+v", list)
		}
		if _, err := s.Credentials().Get(ctx, "org", "u2", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user saw the credential: %v", err)
		}
		if _, err := s.Credentials().Get(ctx, "other", "u1", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org saw the credential: %v", err)
		}
		if err := s.Credentials().Delete(ctx, "org", "u1", "github"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Credentials().Get(ctx, "org", "u1", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a deleted credential is still there: %v", err)
		}
		if err := s.Credentials().Delete(ctx, "org", "u1", "github"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a second delete: %v", err)
		}
	},
	// A routed credential's route is stored beside its secret and
	// replaced with it; Postgres may reorder the JSON, so it is compared
	// decoded. @decision(DL-073)
	"a routed credential's route is kept and replaced with it": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		c := store.UserCredential{ID: store.NewID(t0), OrgID: "org", UserID: "u1", Platform: "route:acme",
			Ciphertext: []byte("sealed"), Nonce: []byte("nonce"), KeyVersion: 1,
			Route: []byte(`{"name":"acme","hosts":["npm.acme.com"],"scheme":"bearer","attach":"always"}`)}
		if err := s.Credentials().Put(ctx, c); err != nil {
			t.Fatal(err)
		}
		decoded := func(b []byte) map[string]any {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("route %q: %v", b, err)
			}
			return m
		}
		got, err := s.Credentials().Get(ctx, "org", "u1", "route:acme")
		if err != nil || !reflect.DeepEqual(decoded(got.Route), decoded(c.Route)) {
			t.Fatalf("got %s, %v", got.Route, err)
		}
		c.Route = []byte(`{"name":"acme","hosts":["npm2.acme.com"],"scheme":"bearer","attach":"asked"}`)
		if err := s.Credentials().Put(ctx, c); err != nil {
			t.Fatal(err)
		}
		list, _ := s.Credentials().ListByUser(ctx, "org", "u1")
		if len(list) != 1 || !reflect.DeepEqual(decoded(list[0].Route), decoded(c.Route)) {
			t.Errorf("list = %+v", list)
		}
		plain := store.UserCredential{ID: store.NewID(t0), OrgID: "org", UserID: "u1", Platform: "github",
			Ciphertext: []byte("x"), Nonce: []byte("n"), KeyVersion: 1}
		if err := s.Credentials().Put(ctx, plain); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Credentials().Get(ctx, "org", "u1", "github"); got.Route != nil {
			t.Errorf("a forge credential has a route: %s", got.Route)
		}
	},
	"returned users, tokens and credentials are copies": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		login := t0
		maxSessions := 3
		created := store.User{ID: "u-copy", OrgID: "org", Email: "c@example.com", LastLoginAt: &login,
			Quota: store.Quota{MaxConcurrentSessions: &maxSessions}}
		if err := s.Users().Create(ctx, created); err != nil {
			t.Fatal(err)
		}
		u, _ := s.Users().Get(ctx, "org", "u-copy")
		*u.LastLoginAt = t0.Add(time.Hour)
		if again, _ := s.Users().Get(ctx, "org", "u-copy"); !again.LastLoginAt.Equal(login) {
			t.Error("a caller's mutation reached the stored user")
		}
		// Every quota field is a pointer, so a shallow copy shares all
		// four with the store — and with the caller who created the row.
		//
		// @decision(DL-009)
		*u.Quota.MaxConcurrentSessions = 99
		if again, _ := s.Users().Get(ctx, "org", "u-copy"); *again.Quota.MaxConcurrentSessions != 3 {
			t.Errorf("a caller's mutation reached the stored quota: %d", *again.Quota.MaxConcurrentSessions)
		}
		if maxSessions != 3 {
			t.Errorf("the store mutated the caller's own struct: %d", maxSessions)
		}
		if err := s.Tokens().Create(ctx, store.APIToken{ID: "t-copy", OrgID: "org", UserID: "u1", Hash: []byte("h-copy")}); err != nil {
			t.Fatal(err)
		}
		tok, _ := s.Tokens().ByHash(ctx, []byte("h-copy"))
		tok.Hash[0] = 'X'
		if _, err := s.Tokens().ByHash(ctx, []byte("h-copy")); err != nil {
			t.Error("a caller's mutation reached the stored token hash")
		}
		if err := s.Credentials().Put(ctx, store.UserCredential{ID: "c-copy", OrgID: "org", UserID: "u1",
			Platform: "p", Ciphertext: []byte("secret"), Nonce: []byte("n")}); err != nil {
			t.Fatal(err)
		}
		cred, _ := s.Credentials().Get(ctx, "org", "u1", "p")
		cred.Ciphertext[0] = 'X'
		if again, _ := s.Credentials().Get(ctx, "org", "u1", "p"); string(again.Ciphertext) != "secret" {
			t.Error("a caller's mutation reached the stored ciphertext")
		}
	},
	"a conflict inside a transaction ends it": func(t *testing.T, s store.Store) {
		// Postgres aborts a transaction on a failed statement, so a
		// caller cannot catch ErrConflict inside Tx and carry on. The
		// memory store would allow it; pinning the stricter semantics
		// here keeps a caller from relying on the looser one.
		ctx := context.Background()
		first := session(store.NewID(t0), "org", "u1", "taken", store.StateRunning)
		if err := s.Sessions().Create(ctx, first); err != nil {
			t.Fatal(err)
		}
		clash := session(store.NewID(t0.Add(time.Second)), "org", "u1", "taken", store.StateRunning)
		after := session(store.NewID(t0.Add(2*time.Second)), "org", "u1", "after", store.StateRunning)
		err := s.Tx(ctx, func(tx store.Store) error {
			if err := tx.Sessions().Create(ctx, clash); !errors.Is(err, store.ErrConflict) {
				t.Errorf("create onto a live name: %v", err)
			}
			// Recovering and writing again must not silently succeed.
			return tx.Sessions().Create(ctx, after)
		})
		if err == nil {
			if _, getErr := s.Sessions().Get(ctx, "org", after.ID); getErr != nil {
				t.Error("the transaction reported success but its write is missing")
			}
			return
		}
		if _, getErr := s.Sessions().Get(ctx, "org", after.ID); !errors.Is(getErr, store.ErrNotFound) {
			t.Error("a failed transaction left a write behind")
		}
	},
	"tx rolls back on panic and refuses to nest": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		a := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		func() {
			defer func() { _ = recover() }()
			_ = s.Tx(ctx, func(tx store.Store) error {
				if err := tx.Sessions().Create(ctx, a); err != nil {
					t.Fatal(err)
				}
				panic("boom")
			})
		}()
		if _, err := s.Sessions().Get(ctx, "org", a.ID); !errors.Is(err, store.ErrNotFound) {
			t.Error("a create from a panicking transaction is visible")
		}
		err := s.Tx(ctx, func(tx store.Store) error {
			return tx.Tx(ctx, func(store.Store) error { return nil })
		})
		if !errors.Is(err, store.ErrNestedTx) {
			t.Errorf("nested Tx: %v", err)
		}
	},
	"tx commits on success and rolls back on error": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		a := session(store.NewID(t0), "org", "u1", "a", store.StateRunning)
		err := s.Tx(ctx, func(tx store.Store) error {
			return tx.Sessions().Create(ctx, a)
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sessions().Get(ctx, "org", a.ID); err != nil {
			t.Fatalf("committed session missing: %v", err)
		}
		b := session(store.NewID(t0.Add(time.Second)), "org", "u1", "b", store.StateRunning)
		boom := errors.New("boom")
		err = s.Tx(ctx, func(tx store.Store) error {
			if err := tx.Sessions().Create(ctx, b); err != nil {
				return err
			}
			a.State = store.StateStopped
			if err := tx.Sessions().Update(ctx, a); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if _, err := s.Sessions().Get(ctx, "org", b.ID); !errors.Is(err, store.ErrNotFound) {
			t.Error("a rolled-back create is visible")
		}
		if got, _ := s.Sessions().Get(ctx, "org", a.ID); got.State != store.StateRunning {
			t.Error("a rolled-back update is visible")
		}
	},

	// @decision(DL-007)
	"allowlist: put, get, list and delete": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		al := s.Allowlist()
		for _, e := range []store.AllowlistEntry{
			{OrgID: "org", Email: "zoe@example.com"},
			{OrgID: "org", Email: "ada@example.com", Admin: true},
		} {
			if err := al.Put(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		got, err := al.Get(ctx, "org", "ada@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Admin || got.OrgID != "org" {
			t.Errorf("got %+v", got)
		}
		if got.CreatedAt.IsZero() {
			t.Error("created_at was not set")
		}
		list, err := al.List(ctx, "org")
		if err != nil {
			t.Fatal(err)
		}
		// Sorted by e-mail, so a listing is stable between reads.
		if len(list) != 2 || list[0].Email != "ada@example.com" || list[1].Email != "zoe@example.com" {
			t.Fatalf("list = %+v", list)
		}
		if err := al.Delete(ctx, "org", "zoe@example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := al.Get(ctx, "org", "zoe@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("get after delete: %v", err)
		}
		if err := al.Delete(ctx, "org", "zoe@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting twice: %v", err)
		}
	},

	// Addresses compare case-insensitively, and the table has to as
	// well or an admin who types Bob@ creates a second
	// entry instead of editing the one they meant.
	//
	// @decision(DL-007)
	"allowlist: the e-mail is a case-insensitive key": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		al := s.Allowlist()
		if err := al.Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "  Bob@Example.COM "}); err != nil {
			t.Fatal(err)
		}
		got, err := al.Get(ctx, "org", "bob@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got.Email != "bob@example.com" {
			t.Errorf("stored as %q, want it normalised", got.Email)
		}
		// A second put in yet another case must edit, not duplicate.
		if err := al.Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "BOB@example.com", Admin: true}); err != nil {
			t.Fatal(err)
		}
		list, err := al.List(ctx, "org")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || !list[0].Admin {
			t.Fatalf("list = %+v, want one admin entry", list)
		}
		if err := al.Delete(ctx, "org", "Bob@EXAMPLE.com"); err != nil {
			t.Errorf("delete in another case: %v", err)
		}
	},

	// @decision(DL-007)
	"allowlist: a put keeps when the address was first allowed": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		al := s.Allowlist()
		first := t0.Add(-48 * time.Hour)
		if err := al.Put(ctx, store.AllowlistEntry{
			OrgID: "org", Email: "ada@example.com", CreatedAt: first,
		}); err != nil {
			t.Fatal(err)
		}
		if err := al.Put(ctx, store.AllowlistEntry{
			OrgID: "org", Email: "ada@example.com", Admin: true, CreatedAt: t0,
		}); err != nil {
			t.Fatal(err)
		}
		got, err := al.Get(ctx, "org", "ada@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Admin {
			t.Error("the put did not grant admin")
		}
		if !got.CreatedAt.Equal(first) {
			t.Errorf("created_at = %v, want the original %v", got.CreatedAt, first)
		}
	},

	// @decision(DL-007)
	"allowlist: entries do not cross orgs": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		al := s.Allowlist()
		if err := al.Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "ada@example.com", Admin: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := al.Get(ctx, "other", "ada@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org can read the entry: %v", err)
		}
		if list, err := al.List(ctx, "other"); err != nil || len(list) != 0 {
			t.Errorf("list for another org = %+v, %v", list, err)
		}
		if n, err := al.CountAdmins(ctx, "other"); err != nil || n != 0 {
			t.Errorf("admins in another org = %d, %v", n, err)
		}
		if err := al.Delete(ctx, "other", "ada@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org can delete the entry: %v", err)
		}
	},

	// CountAdmins is what the "never leave the org without an admin"
	// guard decides on, so it must count admins and nothing else.
	//
	// @decision(DL-007)
	"allowlist: counting admins": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		al := s.Allowlist()
		if n, err := al.CountAdmins(ctx, "org"); err != nil || n != 0 {
			t.Fatalf("empty: %d, %v", n, err)
		}
		for _, e := range []store.AllowlistEntry{
			{OrgID: "org", Email: "ada@example.com", Admin: true},
			{OrgID: "org", Email: "bob@example.com", Admin: true},
			{OrgID: "org", Email: "zoe@example.com"},
		} {
			if err := al.Put(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := al.CountAdmins(ctx, "org"); err != nil || n != 2 {
			t.Fatalf("admins = %d, %v, want 2", n, err)
		}
		// A demotion is a put, and must be counted as one.
		if err := al.Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "bob@example.com"}); err != nil {
			t.Fatal(err)
		}
		if n, err := al.CountAdmins(ctx, "org"); err != nil || n != 1 {
			t.Errorf("after demoting: %d, %v, want 1", n, err)
		}
	},

	// The zero-admin guard reads and writes in one transaction, so a
	// rolled-back change must leave the list as it was.
	//
	// @decision(DL-007)
	"allowlist: a rolled-back change is not visible": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		al := s.Allowlist()
		if err := al.Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "ada@example.com", Admin: true}); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		err := s.Tx(ctx, func(tx store.Store) error {
			if n, err := tx.Allowlist().CountAdmins(ctx, "org"); err != nil || n != 1 {
				t.Errorf("inside the transaction: %d, %v", n, err)
			}
			if err := tx.Allowlist().Delete(ctx, "org", "ada@example.com"); err != nil {
				return err
			}
			return tx.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "zoe@example.com"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := al.Get(ctx, "org", "zoe@example.com"); err != nil {
			t.Fatalf("the committed put is missing: %v", err)
		}
		err = s.Tx(ctx, func(tx store.Store) error {
			if err := tx.Allowlist().Put(ctx, store.AllowlistEntry{
				OrgID: "org", Email: "eve@example.com", Admin: true,
			}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if _, err := al.Get(ctx, "org", "eve@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Error("a rolled-back put is visible")
		}
	},

	// Conversations are idempotent on OpenCode's identifiers: the same
	// read of the sandbox written twice is one set of rows.
	//
	// @decision(DL-017)
	"conversations: upsert keeps identity and fills the title later": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		cs := s.Conversations()
		first, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
			t.Error("timestamps were not set")
		}
		again, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0.Add(time.Second)), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1", Title: "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID {
			t.Errorf("a second upsert minted a new id %s, want %s", again.ID, first.ID)
		}
		if again.Title != "hello" {
			t.Errorf("title = %q, want hello", again.Title)
		}
		blank, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1",
		})
		if err != nil || blank.Title != "hello" {
			t.Errorf("an empty title overwrote a known one: %+v, %v", blank, err)
		}
		// The info is kept whole and, like the title, only replaced by
		// one that says something. @decision(DL-089)
		info := []byte(`{"id":"ses_1","time":{"created":1,"updated":2}}`)
		withInfo, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1", Info: info,
		})
		if err != nil || string(withInfo.Info) != string(info) || withInfo.Title != "hello" {
			t.Errorf("the info was not kept: %+v, %v", withInfo, err)
		}
		if kept, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1",
		}); err != nil || string(kept.Info) != string(info) {
			t.Errorf("an upsert with no info dropped the stored one: %q, %v", kept.Info, err)
		}
		list, err := cs.ListConversations(ctx, "org", sess.ID)
		if err != nil || len(list) != 1 {
			t.Fatalf("list: %+v, %v", list, err)
		}
		if other, _ := cs.ListConversations(ctx, "org-2", sess.ID); len(other) != 0 {
			t.Error("another org saw the conversation")
		}
	},
	// Oldest first, by created_at rather than by id: a ULID is only
	// monotonic across milliseconds, so two conversations minted in the
	// same one sort by their random suffix, which is why the ids here
	// descend while the times ascend. `spov attach` opens the first of
	// this list, and `jobConversation` reads the same rows.
	//
	// @decision(DL-025)
	"conversations: list oldest first, by creation time and not by id": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		cs := s.Conversations()
		// Ids in one millisecond so they cannot carry the order, and
		// deliberately descending so an id sort is visibly wrong.
		ids := []string{store.NewID(t0), store.NewID(t0), store.NewID(t0)}
		slices.Sort(ids)
		slices.Reverse(ids)
		for i, want := range []string{"ses_oldest", "ses_middle", "ses_newest"} {
			if _, err := cs.UpsertConversation(ctx, store.Conversation{
				ID: ids[i], OrgID: "org", SessionID: sess.ID, OpenCodeID: want,
				CreatedAt: t0.Add(time.Duration(i) * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
		}
		list, err := cs.ListConversations(ctx, "org", sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range list {
			got = append(got, c.OpenCodeID)
		}
		if want := []string{"ses_oldest", "ses_middle", "ses_newest"}; !slices.Equal(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	},
	"conversations: messages upsert by opencode id and count what changed": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		cs := s.Conversations()
		conv := conversation(t, s, sess, "ses_1")
		msg := func(id, ocid, text string, at time.Time) store.Message {
			return store.Message{ID: id, OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
				OpenCodeID: ocid, Role: "user", Text: text, At: at}
		}
		a, b := store.NewID(t0), store.NewID(t0.Add(time.Second))
		n, err := cs.PutMessages(ctx, []store.Message{msg(a, "msg_a", "hi", t0), msg(b, "msg_b", "", time.Time{})})
		if err != nil || n != 2 {
			t.Fatalf("first put: %d, %v", n, err)
		}
		n, err = cs.PutMessages(ctx, []store.Message{msg(store.NewID(t0), "msg_a", "hi", t0)})
		if err != nil || n != 0 {
			t.Errorf("the same message again changed %d rows, %v", n, err)
		}
		n, err = cs.PutMessages(ctx, []store.Message{msg(store.NewID(t0), "msg_b", "hello there", t0.Add(time.Minute))})
		if err != nil || n != 1 {
			t.Errorf("a completed message changed %d rows, %v", n, err)
		}
		got, err := cs.ListMessages(ctx, "org", sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != a || got[1].ID != b {
			t.Fatalf("list = %+v", got)
		}
		if got[1].Text != "hello there" || !got[1].At.Equal(t0.Add(time.Minute)) {
			t.Errorf("the update did not land: %+v", got[1])
		}
		if got[0].CreatedAt.IsZero() {
			t.Error("created_at was not set")
		}
		if other, _ := cs.ListMessages(ctx, "org-2", sess.ID); len(other) != 0 {
			t.Error("another org saw the messages")
		}
	},
	"conversations: a message with no time sorts first": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		conv := conversation(t, s, sess, "ses_1")
		timed := store.Message{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			OpenCodeID: "msg_t", Text: "timed", At: t0}
		untimed := store.Message{ID: store.NewID(t0.Add(time.Second)), OrgID: "org", SessionID: sess.ID,
			ConversationID: conv.ID, OpenCodeID: "msg_u", Text: "untimed"}
		if _, err := s.Conversations().PutMessages(ctx, []store.Message{timed, untimed}); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Conversations().ListMessages(ctx, "org", sess.ID)
		if len(got) != 2 || got[0].OpenCodeID != "msg_u" || !got[0].At.IsZero() {
			t.Errorf("order = %+v", got)
		}
	},
	"conversations: a message for a missing conversation is refused": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		_, err := s.Conversations().PutMessages(ctx, []store.Message{{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: store.NewID(t0),
			OpenCodeID: "msg_x", Text: "orphan",
		}})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	},
	"conversations: last messages, including a session with none": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		talky := runningSession(t, s, "org", "u1", "alpha")
		quiet := runningSession(t, s, "org", "u1", "beta")
		conv := conversation(t, s, talky, "ses_1")
		msgs := []store.Message{
			{ID: store.NewID(t0), OrgID: "org", SessionID: talky.ID, ConversationID: conv.ID, OpenCodeID: "m1", Text: "first", At: t0},
			{ID: store.NewID(t0.Add(time.Second)), OrgID: "org", SessionID: talky.ID, ConversationID: conv.ID, OpenCodeID: "m2", Text: "last", At: t0.Add(time.Hour)},
			{ID: store.NewID(t0.Add(2 * time.Second)), OrgID: "org", SessionID: talky.ID, ConversationID: conv.ID, OpenCodeID: "m3", Text: "middle", At: t0.Add(time.Minute)},
		}
		if _, err := s.Conversations().PutMessages(ctx, msgs); err != nil {
			t.Fatal(err)
		}
		got, err := s.Conversations().LastMessages(ctx, "org", []string{talky.ID, quiet.ID, "nope"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[talky.ID].Text != "last" {
			t.Errorf("last = %+v", got)
		}
		if none, err := s.Conversations().LastMessages(ctx, "org", nil); err != nil || len(none) != 0 {
			t.Errorf("no sessions: %+v, %v", none, err)
		}
		if other, _ := s.Conversations().LastMessages(ctx, "org-2", []string{talky.ID}); len(other) != 0 {
			t.Error("another org saw the last message")
		}
	},
	"conversations: a duplicate frame is one row and pos only grows": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		conv := conversation(t, s, sess, "ses_1")
		cs := s.Conversations()
		frame := func(seq int64) store.Event {
			return store.Event{ID: store.NewID(t0.Add(time.Duration(seq) * time.Millisecond)), OrgID: "org",
				SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode, Seq: seq,
				Name: "session.step.started", At: t0, Raw: []byte(`{"seq":` + string(rune('0'+seq)) + `}`)}
		}
		written, err := cs.PutEvents(ctx, []store.Event{frame(1), frame(2), frame(3)})
		if err != nil || len(written) != 3 {
			t.Fatalf("first put: %d rows, %v", len(written), err)
		}
		for i := 1; i < len(written); i++ {
			if written[i].Pos <= written[i-1].Pos {
				t.Errorf("pos did not grow: %d after %d", written[i].Pos, written[i-1].Pos)
			}
		}
		if written[0].Pos <= 0 || written[0].CreatedAt.IsZero() {
			t.Errorf("pos or created_at unset: %+v", written[0])
		}
		again, err := cs.PutEvents(ctx, []store.Event{frame(2), frame(4)})
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 1 || again[0].Seq != 4 || again[0].Pos <= written[2].Pos {
			t.Errorf("a redelivered frame was not skipped, or pos went backwards: %+v", again)
		}
		all, err := cs.ListEvents(ctx, "org", sess.ID, 0, 0)
		if err != nil || len(all) != 4 {
			t.Fatalf("all: %d, %v", len(all), err)
		}
		if string(all[0].Raw) != `{"seq":1}` {
			t.Errorf("raw = %q", all[0].Raw)
		}
		page, _ := cs.ListEvents(ctx, "org", sess.ID, all[1].Pos, 1)
		if len(page) != 1 || page[0].Seq != 3 {
			t.Errorf("after pos %d limit 1 = %+v", all[1].Pos, page)
		}
		if other, _ := cs.ListEvents(ctx, "org-2", sess.ID, 0, 0); len(other) != 0 {
			t.Error("another org saw the events")
		}
	},
	"conversations: an opencode event may not claim spov's namespace": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		conv := conversation(t, s, sess, "ses_1")
		cs := s.Conversations()
		_, err := cs.PutEvents(ctx, []store.Event{{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			Source: store.SourceOpenCode, Seq: 1, Name: store.SpovNamespace + "recorder.gap",
			Text: "the sandbox speaking as spov", At: t0,
		}})
		if !errors.Is(err, store.ErrInvalidEvent) {
			t.Fatalf("a sandbox frame took spov's namespace: %v", err)
		}
		if got, _ := cs.ListEvents(ctx, "org", sess.ID, 0, 0); len(got) != 0 {
			t.Errorf("it was stored anyway: %+v", got)
		}
		// spov's own events are exactly the ones that may.
		if _, err := cs.PutEvents(ctx, []store.Event{{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID,
			Source: store.SourceSpov, Name: store.SpovNamespace + "recorder.gap", At: t0,
		}}); err != nil {
			t.Errorf("spov could not write its own marker: %v", err)
		}
	},
	"conversations: the last event is the newest by pos, and ErrNotFound when there is none": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		cs := s.Conversations()
		if _, err := cs.LastEvent(ctx, "org", sess.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a session with no events answered %v", err)
		}
		conv := conversation(t, s, sess, "ses_1")
		for _, seq := range []int64{1, 2} {
			if _, err := cs.PutEvents(ctx, []store.Event{{
				ID: store.NewID(t0.Add(time.Duration(seq) * time.Millisecond)), OrgID: "org", SessionID: sess.ID,
				ConversationID: conv.ID, Source: store.SourceOpenCode, Seq: seq, Name: "session.step.started", At: t0,
			}}); err != nil {
				t.Fatal(err)
			}
		}
		marker, err := cs.PutEvents(ctx, []store.Event{{
			ID: store.NewID(t0.Add(time.Second)), OrgID: "org", SessionID: sess.ID,
			Source: store.SourceSpov, Name: "spov.recorder.stopped", Text: "gave up", At: t0,
		}})
		if err != nil || len(marker) != 1 {
			t.Fatalf("marker: %d rows, %v", len(marker), err)
		}
		last, err := cs.LastEvent(ctx, "org", sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if last.Name != "spov.recorder.stopped" || last.Text != "gave up" || last.Pos != marker[0].Pos {
			t.Errorf("last event = %+v", last)
		}
		if _, err := cs.LastEvent(ctx, "org-2", sess.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org saw the last event: %v", err)
		}
	},
	"conversations: a spov marker needs no conversation and never conflicts": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		cs := s.Conversations()
		mark := func(id string) store.Event {
			return store.Event{ID: id, OrgID: "org", SessionID: sess.ID, Source: store.SourceSpov,
				Name: "spov.recorder.gap", Text: "the stream broke", At: t0}
		}
		got, err := cs.PutEvents(ctx, []store.Event{mark(store.NewID(t0)), mark(store.NewID(t0.Add(time.Second)))})
		if err != nil || len(got) != 2 {
			t.Fatalf("two markers: %d, %v", len(got), err)
		}
		if got[0].ConversationID != "" || got[0].Seq != 0 {
			t.Errorf("marker = %+v", got[0])
		}
		bad := []store.Event{
			{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, Source: store.SourceOpenCode, Name: "x", Seq: 1},
			{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, Source: "elsewhere", Name: "x"},
		}
		for _, ev := range bad {
			if _, err := cs.PutEvents(ctx, []store.Event{ev}); !errors.Is(err, store.ErrInvalidEvent) {
				t.Errorf("%+v: err = %v, want ErrInvalidEvent", ev, err)
			}
		}
		if _, err := cs.PutEvents(ctx, []store.Event{mark(got[0].ID)}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a reused id: %v, want ErrConflict", err)
		}
	},
	"conversations: a row must agree with its conversation's org and session": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		other := runningSession(t, s, "org", "u1", "beta")
		conv := conversation(t, s, sess, "ses_1")
		cs := s.Conversations()
		if _, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0), OrgID: "org", SessionID: store.NewID(t0), OpenCodeID: "ses_x",
		}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a conversation for a missing session: %v, want ErrNotFound", err)
		}
		if _, err := cs.UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(t0), OrgID: "org-2", SessionID: sess.ID, OpenCodeID: "ses_x",
		}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a conversation under another org: %v, want ErrNotFound", err)
		}
		strayMessage := store.Message{ID: store.NewID(t0), OrgID: "org", SessionID: other.ID,
			ConversationID: conv.ID, OpenCodeID: "msg_s", Text: "stray"}
		if _, err := cs.PutMessages(ctx, []store.Message{strayMessage}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a message under another session: %v, want ErrNotFound", err)
		}
		if got, _ := cs.ListMessages(ctx, "org", other.ID); len(got) != 0 {
			t.Error("the stray message was listed under the other session")
		}
		strayEvent := store.Event{ID: store.NewID(t0), OrgID: "org", SessionID: other.ID, ConversationID: conv.ID,
			Source: store.SourceOpenCode, Seq: 1, Name: "x", At: t0}
		if _, err := cs.PutEvents(ctx, []store.Event{strayEvent}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an event under another session: %v, want ErrNotFound", err)
		}
		lostMarker := store.Event{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: store.NewID(t0),
			Source: store.SourceSpov, Name: "spov.recorder.gap", At: t0}
		if _, err := cs.PutEvents(ctx, []store.Event{lostMarker}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a marker naming a missing conversation: %v, want ErrNotFound", err)
		}
		placedMarker := store.Event{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			Source: store.SourceSpov, Name: "spov.recorder.gap", At: t0}
		if got, err := cs.PutEvents(ctx, []store.Event{placedMarker}); err != nil || len(got) != 1 || got[0].ConversationID != conv.ID {
			t.Errorf("a marker placed in a conversation: %+v, %v", got, err)
		}
	},
	"conversations: a bad batch writes nothing": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		conv := conversation(t, s, sess, "ses_1")
		cs := s.Conversations()
		ev := func(id string, seq int64) store.Event {
			return store.Event{ID: id, OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
				Source: store.SourceOpenCode, Seq: seq, Name: "x", At: t0}
		}
		first, err := cs.PutEvents(ctx, []store.Event{ev(store.NewID(t0), 1)})
		if err != nil || len(first) != 1 {
			t.Fatal(err)
		}
		if _, err := cs.PutEvents(ctx, []store.Event{ev(store.NewID(t0), 2), ev(first[0].ID, 3)}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a reused id in the batch: %v, want ErrConflict", err)
		}
		untimed := ev(store.NewID(t0), 4)
		untimed.At = time.Time{}
		if _, err := cs.PutEvents(ctx, []store.Event{ev(store.NewID(t0), 5), untimed}); !errors.Is(err, store.ErrInvalidEvent) {
			t.Fatalf("an event with no time: %v, want ErrInvalidEvent", err)
		}
		got, _ := cs.ListEvents(ctx, "org", sess.ID, 0, 0)
		if len(got) != 1 {
			t.Fatalf("a failed batch left rows behind: %+v", got)
		}
		// pos was not consumed by the failed batches, in the sense that
		// matters: the next write still lands after everything stored.
		next, err := cs.PutEvents(ctx, []store.Event{ev(store.NewID(t0), 6), ev(store.NewID(t0.Add(time.Second)), 6)})
		if err != nil || len(next) != 1 || next[0].Pos <= first[0].Pos {
			t.Errorf("a batch repeating its own frame: %+v, %v", next, err)
		}
		if none, err := cs.PutEvents(ctx, nil); err != nil || len(none) != 0 {
			t.Errorf("an empty batch: %+v, %v", none, err)
		}
		if _, err := cs.PutMessages(ctx, []store.Message{
			{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID, OpenCodeID: "m1", Text: "ok"},
			{ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: store.NewID(t0), OpenCodeID: "m2", Text: "lost"},
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a message batch with a bad row: %v", err)
		}
		if msgs, _ := cs.ListMessages(ctx, "org", sess.ID); len(msgs) != 0 {
			t.Errorf("a failed message batch left rows behind: %+v", msgs)
		}
	},
	"conversations: a rolled-back write is not visible": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "alpha")
		conv := conversation(t, s, sess, "ses_1")
		boom := errors.New("boom")
		err := s.Tx(ctx, func(tx store.Store) error {
			_, err := tx.Conversations().PutEvents(ctx, []store.Event{{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
				Source: store.SourceOpenCode, Seq: 1, Name: "x", At: t0,
			}})
			if err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if got, _ := s.Conversations().ListEvents(ctx, "org", sess.ID, 0, 0); len(got) != 0 {
			t.Error("a rolled-back event is visible")
		}
		err = s.Tx(ctx, func(tx store.Store) error {
			c, err := tx.Conversations().UpsertConversation(ctx, store.Conversation{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_2", Title: "doomed",
			})
			if err != nil {
				return err
			}
			if _, err := tx.Conversations().PutMessages(ctx, []store.Message{{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: c.ID, OpenCodeID: "m", Text: "doomed",
			}}); err != nil {
				return err
			}
			if _, err := tx.Conversations().UpsertConversation(ctx, store.Conversation{
				ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1", Title: "renamed",
			}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		convs, _ := s.Conversations().ListConversations(ctx, "org", sess.ID)
		if len(convs) != 1 || convs[0].Title != "" {
			t.Errorf("a rolled-back conversation write is visible: %+v", convs)
		}
		if msgs, _ := s.Conversations().ListMessages(ctx, "org", sess.ID); len(msgs) != 0 {
			t.Error("a rolled-back message is visible")
		}
	},
	"events: ephemeral frames are kept, and are not deduplicated against each other": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		conv := conversation(t, s, sess, "ses_1")
		ev := func(id, name string) store.Event {
			return store.Event{
				ID: id, OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
				Source: store.SourceEphemeral, Name: name, At: t0,
				Raw: []byte(`{"type":"` + name + `"}`),
			}
		}
		// Two permission.asked frames, neither with a sequence. Under
		// the durable log's dedup key they would collide and the second
		// would be swallowed — and the second is the one that says a
		// job is parked on a new question.
		written, err := s.Conversations().PutEvents(ctx, []store.Event{
			ev("e1", "permission.asked"),
			ev("e2", "permission.asked"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(written) != 2 {
			t.Fatalf("wrote %d ephemeral frames, want 2: the second was swallowed as a duplicate of the first", len(written))
		}
		if written[0].Pos >= written[1].Pos {
			t.Errorf("ephemeral frames did not get increasing positions: %d, %d", written[0].Pos, written[1].Pos)
		}
		got, err := s.Conversations().ListEvents(ctx, "org", sess.ID, 0, 0)
		if err != nil || len(got) != 2 {
			t.Fatalf("ListEvents = %d events, %v", len(got), err)
		}
		for _, e := range got {
			if e.Source != store.SourceEphemeral {
				t.Errorf("source = %q, want ephemeral", e.Source)
			}
			if e.Seq != 0 {
				t.Errorf("an ephemeral frame has seq %d, want 0", e.Seq)
			}
			// The frame is kept whole, because what the supervisor
			// needs out of it (the per_… id) is not in the projection.
			if len(e.Raw) == 0 {
				t.Error("an ephemeral frame was stored without its raw body")
			}
		}
	},
	"events: an ephemeral frame is refused without a conversation, or with a sequence": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		conv := conversation(t, s, sess, "ses_1")
		base := store.Event{
			ID: "e1", OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			Source: store.SourceEphemeral, Name: "permission.asked", At: t0,
		}
		noConv := base
		noConv.ConversationID = ""
		if _, err := s.Conversations().PutEvents(ctx, []store.Event{noConv}); !errors.Is(err, store.ErrInvalidEvent) {
			t.Errorf("an ephemeral frame with no conversation = %v, want ErrInvalidEvent", err)
		}
		// A sequence would mean it belongs in the durable log, where it
		// would be deduplicated against real frames.
		withSeq := base
		withSeq.Seq = 3
		if _, err := s.Conversations().PutEvents(ctx, []store.Event{withSeq}); !errors.Is(err, store.ErrInvalidEvent) {
			t.Errorf("an ephemeral frame with a sequence = %v, want ErrInvalidEvent", err)
		}
		// And it may not claim spov's own voice by name.
		spovName := base
		spovName.Name = store.SpovNamespace + "recorder.gap"
		if _, err := s.Conversations().PutEvents(ctx, []store.Event{spovName}); !errors.Is(err, store.ErrInvalidEvent) {
			t.Errorf("an ephemeral frame in spov's namespace = %v, want ErrInvalidEvent", err)
		}
	},
	"events: a durable frame is still deduplicated by its sequence": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		conv := conversation(t, s, sess, "ses_1")
		durable := func(id string, seq int64) store.Event {
			return store.Event{
				ID: id, OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
				Source: store.SourceOpenCode, Seq: seq, Name: "session.execution.succeeded", At: t0,
			}
		}
		// Making seq nullable must not weaken the rule it exists for.
		if _, err := s.Conversations().PutEvents(ctx, []store.Event{durable("d1", 5)}); err != nil {
			t.Fatal(err)
		}
		again, err := s.Conversations().PutEvents(ctx, []store.Event{durable("d2", 5)})
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Errorf("a repeated seq wrote %d rows, want 0", len(again))
		}
	},
	"messages: the raw frame round-trips": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		conv := conversation(t, s, sess, "ses_1")
		raw := []byte(`{"id":"msg_1","parts":[{"type":"tool","state":{"status":"error"}}]}`)
		msg := store.Message{
			ID: store.NewID(t0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			OpenCodeID: "msg_1", Role: "assistant", Text: "hello", At: t0, Raw: raw,
		}
		if _, err := s.Conversations().PutMessages(ctx, []store.Message{msg}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Conversations().ListMessages(ctx, "org", sess.ID)
		if err != nil || len(got) != 1 {
			t.Fatalf("ListMessages = %d, %v", len(got), err)
		}
		// Gate 3 re-projects tool parts out of this, so it has to come
		// back byte for byte.
		if string(got[0].Raw) != string(raw) {
			t.Errorf("raw = %q, want %q", got[0].Raw, raw)
		}
		// A message stored without a frame reads back empty, not nil,
		// on both stores.
		bare := msg
		bare.ID, bare.OpenCodeID, bare.Raw = store.NewID(t0.Add(time.Second)), "msg_2", nil
		if _, err := s.Conversations().PutMessages(ctx, []store.Message{bare}); err != nil {
			t.Fatal(err)
		}
		all, _ := s.Conversations().ListMessages(ctx, "org", sess.ID)
		for _, m := range all {
			if m.Raw == nil {
				t.Errorf("message %s came back with a nil raw rather than an empty one", m.OpenCodeID)
			}
		}
		// A caller writing through the returned frame must not reach
		// the stored row.
		if len(got[0].Raw) > 0 {
			got[0].Raw[0] = 'X'
			after, _ := s.Conversations().ListMessages(ctx, "org", sess.ID)
			for _, m := range after {
				if m.OpenCodeID == "msg_1" && string(m.Raw) != string(raw) {
					t.Error("the store handed out a slice into its own row")
				}
			}
		}
	},
	"jobs: a job is created, read back and listed": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Goal != j.Goal || got.Kind != j.Kind || got.State != store.JobQueued {
			t.Errorf("job = %+v", got)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Errorf("the store did not stamp the job: %+v", got)
		}
		// A job is not started or stopped until it says so.
		if got.StartedAt != nil || got.StoppedAt != nil {
			t.Errorf("a queued job is already started or stopped: %+v", got)
		}
		if bySess, err := s.Jobs().BySession(ctx, "org", sess.ID); err != nil || bySess.ID != j.ID {
			t.Errorf("BySession = %+v, %v", bySess, err)
		}
		// The session list's decoration, in one query for the page.
		//
		// @decision(DL-060)
		batch, err := s.Jobs().BySessions(ctx, "org", []string{sess.ID, "ses_no_such_thing"})
		if err != nil {
			t.Errorf("BySessions: %v", err)
		}
		if got, ok := batch[sess.ID]; !ok || got.ID != j.ID {
			t.Errorf("BySessions[%s] = %+v, %v; want the session's job", sess.ID, got, ok)
		}
		// A session with no job is ABSENT rather than present and
		// empty: the caller tests presence to decide whether to draw a
		// badge at all.
		if _, ok := batch["ses_no_such_thing"]; ok {
			t.Error("BySessions invented an entry for a session with no job")
		}
		// An empty request is answered without a query and without a
		// nil map, so a caller can range over it.
		none, err := s.Jobs().BySessions(ctx, "org", nil)
		if err != nil || len(none) != 0 {
			t.Errorf("BySessions(nil) = %+v, %v; want an empty map", none, err)
		}
		list, err := s.Jobs().List(ctx, "org", store.JobFilter{})
		if err != nil || len(list) != 1 || list[0].ID != j.ID {
			t.Errorf("List = %+v, %v", list, err)
		}
	},
	"jobs: another org cannot see or reach one": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Jobs().Get(ctx, "other", j.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get across orgs = %v", err)
		}
		if _, err := s.Jobs().BySession(ctx, "other", sess.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("BySession across orgs = %v", err)
		}
		if list, _ := s.Jobs().List(ctx, "other", store.JobFilter{}); len(list) != 0 {
			t.Errorf("List across orgs = %+v", list)
		}
		// The batch decoration takes session ids from the caller, so an
		// org that guessed one must still get nothing back.
		//
		// @decision(DL-060)
		if got, _ := s.Jobs().BySessions(ctx, "other", []string{sess.ID}); len(got) != 0 {
			t.Errorf("BySessions across orgs = %+v", got)
		}
		// An update from the wrong org must not reach the row either.
		wrong := j
		wrong.OrgID = "other"
		wrong.State = store.JobProvisioning
		if err := s.Jobs().Update(ctx, wrong); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update across orgs = %v", err)
		}
	},
	"jobs: a duplicate id, and a second job for one session, are conflicts": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		if err := s.Jobs().Create(ctx, j); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a duplicate id = %v", err)
		}
		// A session running two loops has two things continuing it, and
		// neither would know about the other.
		second := job(store.NewID(t0.Add(time.Second)), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, second); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a second job for one session = %v", err)
		}
	},
	"jobs: the machine allows its transitions and refuses the rest": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		// queued does not reach running without provisioning first.
		j.State = store.JobRunning
		if err := s.Jobs().Update(ctx, j); !errors.Is(err, store.ErrInvalidTransition) {
			t.Errorf("queued -> running = %v, want ErrInvalidTransition", err)
		}
		// And the refusal changed nothing.
		if got, _ := s.Jobs().Get(ctx, "org", j.ID); got.State != store.JobQueued {
			t.Errorf("a refused transition moved the job to %s", got.State)
		}
		for _, to := range []store.JobState{store.JobProvisioning, store.JobRunning} {
			j.State = to
			if err := s.Jobs().Update(ctx, j); err != nil {
				t.Fatalf("-> %s: %v", to, err)
			}
		}
		// Running stamps StartedAt, once.
		got, _ := s.Jobs().Get(ctx, "org", j.ID)
		if got.StartedAt == nil {
			t.Fatal("reaching running did not stamp StartedAt")
		}
		started := *got.StartedAt
		// A park and a release keep the time the job first started.
		j.State = store.JobBlocked
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		j.State = store.JobRunning
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatalf("blocked -> running: %v", err)
		}
		if got, _ := s.Jobs().Get(ctx, "org", j.ID); !got.StartedAt.Equal(started) {
			t.Errorf("StartedAt moved on a re-entry to running: %v -> %v", started, got.StartedAt)
		}
		// A terminal job stops, and cannot move again.
		j.State = store.JobDelivering
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		j.State = store.JobSucceeded
		j.Unfinished = "the docs"
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		done, _ := s.Jobs().Get(ctx, "org", j.ID)
		if done.StoppedAt == nil || done.Unfinished != "the docs" {
			t.Errorf("a succeeded job = %+v", done)
		}
		j.State = store.JobRunning
		if err := s.Jobs().Update(ctx, j); !errors.Is(err, store.ErrInvalidTransition) {
			t.Errorf("succeeded -> running = %v, want ErrInvalidTransition", err)
		}
	},
	// A steered job reaches delivering on a declaration, and that edge
	// has to exist in the store or the supervisor's write is refused
	// and the job wedges.
	//
	// @decision(DL-048)
	"jobs: a steered job can be delivered, and so can a blocked one": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		for _, to := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobSteered, store.JobDelivering} {
			j.State = to
			if err := s.Jobs().Update(ctx, j); err != nil {
				t.Fatalf("-> %s: %v", to, err)
			}
		}
		// A blocked job is delivered at its wall clock (DL-049), so the
		// edge has to exist in the store too.
		b := job(store.NewID(t0.Add(time.Second)), "org", "u1", runningSession(t, s, "org", "u1", "s2").ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		for _, to := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobBlocked, store.JobDelivering} {
			b.State = to
			if err := s.Jobs().Update(ctx, b); err != nil {
				t.Fatalf("blocked job -> %s: %v", to, err)
			}
		}
	},
	// The wall clock's two fields survive the store, and entering running
	// stamps the clock while a park keeps it.
	//
	// @decision(DL-049)
	"jobs: the wall clock is stored and starts on entering running": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		j.MaxDuration = 45 * time.Minute
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Jobs().Get(ctx, "org", j.ID)
		if got.MaxDuration != 45*time.Minute {
			t.Errorf("MaxDuration = %s after a round trip, want 45m", got.MaxDuration)
		}
		if got.ClockStartedAt != nil {
			t.Errorf("a queued job's clock started at %v", got.ClockStartedAt)
		}
		for _, to := range []store.JobState{store.JobProvisioning, store.JobRunning} {
			j.State = to
			if err := s.Jobs().Update(ctx, j); err != nil {
				t.Fatalf("-> %s: %v", to, err)
			}
		}
		got, _ = s.Jobs().Get(ctx, "org", j.ID)
		if got.ClockStartedAt == nil || got.StartedAt == nil || !got.ClockStartedAt.Equal(*got.StartedAt) {
			t.Fatalf("the first entry into running: clock %v, started %v; want both, equal",
				got.ClockStartedAt, got.StartedAt)
		}
		clock := *got.ClockStartedAt
		j.State = store.JobBlocked
		// Fixed at create: an update carrying another value keeps it.
		j.MaxDuration = 5 * time.Minute
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Jobs().Get(ctx, "org", j.ID); got.ClockStartedAt == nil || !got.ClockStartedAt.Equal(clock) {
			t.Errorf("parking moved the clock: %v -> %v", clock, got.ClockStartedAt)
		}
		if got, _ := s.Jobs().Get(ctx, "org", j.ID); got.MaxDuration != 45*time.Minute {
			t.Errorf("an update changed MaxDuration to %s; it is fixed at create", got.MaxDuration)
		}
		// Leaving the counting states stops the clock and banks the
		// stretch, which the store keeps.
		//
		// @decision(DL-049)
		j.State = store.JobDelivering
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Jobs().Get(ctx, "org", j.ID)
		if got.ClockStartedAt != nil || got.ClockUsed < 0 || got.ClockUsed > time.Minute {
			t.Errorf("delivering: clock %v, used %s; want stopped, with the short stretch banked", got.ClockStartedAt, got.ClockUsed)
		}
	},
	// A banked stretch survives the store in whole seconds, and a resume
	// starts a new stretch without losing it: what a restarted replica
	// reads back is the time the job has used.
	//
	// @decision(DL-049)
	"jobs: the time a job has used is banked and kept across a resume": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobRunning)
		started := time.Now().Add(-10 * time.Minute)
		j.MaxDuration, j.ClockStartedAt, j.ClockUsed = time.Hour, &started, 5*time.Minute
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		j.State = store.JobSteered
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Jobs().Get(ctx, "org", j.ID)
		if got.ClockStartedAt != nil || got.ClockUsed < 15*time.Minute || got.ClockUsed > 16*time.Minute {
			t.Fatalf("steered: clock %v, used %s; want stopped with about 15m banked", got.ClockStartedAt, got.ClockUsed)
		}
		if got.ClockUsed != got.ClockUsed.Truncate(time.Second) {
			t.Errorf("used %s is not whole seconds", got.ClockUsed)
		}
		banked := got.ClockUsed
		got.State = store.JobRunning
		if err := s.Jobs().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		again, _ := s.Jobs().Get(ctx, "org", j.ID)
		if again.ClockStartedAt == nil || again.ClockUsed != banked {
			t.Errorf("resumed: clock %v, used %s; want a new stretch and %s kept", again.ClockStartedAt, again.ClockUsed, banked)
		}
	},
	// Of two callers stopping one session, exactly one makes the move,
	// so a stop is counted once (DL-011).
	//
	// @decision(DL-011)
	"sessions: only the first stop of a live session takes": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		at := t0.Add(time.Hour)
		took, err := s.Sessions().MarkStopped(ctx, "org", sess.ID, store.StateStopped, at, "")
		if err != nil || !took {
			t.Fatalf("the first stop = %v, %v; want it to take", took, err)
		}
		took, err = s.Sessions().MarkStopped(ctx, "org", sess.ID, store.StateFailed, at.Add(time.Minute), "late")
		if err != nil || took {
			t.Errorf("a second stop = %v, %v; want it refused quietly", took, err)
		}
		got, err := s.Sessions().Get(ctx, "org", sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != store.StateStopped || got.Error != "" || got.StoppedAt == nil || !got.StoppedAt.Equal(at) {
			t.Errorf("the row is %s %q stopped %v; want the first stop's", got.State, got.Error, got.StoppedAt)
		}
		other := runningSession(t, s, "org", "u1", "s2")
		if took, err := s.Sessions().MarkStopped(ctx, "other-org", other.ID, store.StateStopped, at, ""); err != nil || took {
			t.Errorf("another org's stop = %v, %v; want it refused", took, err)
		}
		if got, _ := s.Sessions().Get(ctx, "org", other.ID); got.State != store.StateRunning {
			t.Errorf("another org's stop moved the session to %s", got.State)
		}
	},
	"jobs: steered is a suspension, not an end": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		for _, to := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobSteered} {
			j.State = to
			if err := s.Jobs().Update(ctx, j); err != nil {
				t.Fatalf("-> %s: %v", to, err)
			}
		}
		got, _ := s.Jobs().Get(ctx, "org", j.ID)
		if got.StoppedAt != nil {
			t.Error("a steered job is stopped: it is a suspension, and its sandbox is still up")
		}
		// A steered job is still live, so it still shows up as work in
		// flight to a restarted supervisor.
		live, err := s.Jobs().ListLive(ctx)
		if err != nil || len(live) != 1 || live[0].ID != j.ID {
			t.Errorf("ListLive = %+v, %v", live, err)
		}
		// The human hands it back.
		j.State = store.JobRunning
		if err := s.Jobs().Update(ctx, j); err != nil {
			t.Errorf("steered -> running: %v", err)
		}
	},
	// @decision(DL-012)
	"jobs: listing pages newest first by an id cursor, and counts the whole filter": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		var made []store.Job
		for i := range 4 {
			sess := runningSession(t, s, "org", "u1", fmt.Sprintf("pj%d", i))
			j := job(store.NewID(t0.Add(time.Duration(i)*time.Second)), "org", "u1", sess.ID, store.JobQueued)
			if err := s.Jobs().Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			made = append(made, j)
		}
		first, err := s.Jobs().List(ctx, "org", store.JobFilter{Limit: 3})
		if err != nil || len(first) != 3 || first[0].ID != made[3].ID || first[2].ID != made[1].ID {
			t.Fatalf("first page = %+v, %v", first, err)
		}
		next, _ := s.Jobs().List(ctx, "org", store.JobFilter{Limit: 3, Before: first[2].ID})
		if len(next) != 1 || next[0].ID != made[0].ID {
			t.Errorf("second page = %+v, want only %s", next, made[0].ID)
		}
		n, err := s.Jobs().Count(ctx, "org", store.JobFilter{States: []store.JobState{store.JobQueued}, Limit: 1, Before: made[1].ID})
		if err != nil || n != 4 {
			t.Errorf("count = %d, %v; want 4, ignoring the page", n, err)
		}
		if n, _ := s.Jobs().Count(ctx, "org", store.JobFilter{States: []store.JobState{store.JobFailed}}); n != 0 {
			t.Errorf("count of failed = %d, want 0", n)
		}
	},
	"jobs: listing filters by state and session, and live crosses orgs": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		a := runningSession(t, s, "org", "u1", "s1")
		b := runningSession(t, s, "org", "u1", "s2")
		other := runningSession(t, s, "other", "u2", "s3")

		ja := job(store.NewID(t0), "org", "u1", a.ID, store.JobQueued)
		jb := job(store.NewID(t0.Add(time.Second)), "org", "u1", b.ID, store.JobQueued)
		jo := job(store.NewID(t0.Add(2*time.Second)), "other", "u2", other.ID, store.JobQueued)
		for _, j := range []store.Job{ja, jb, jo} {
			if err := s.Jobs().Create(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		// Finish one, so it drops out of the live listing.
		jb.State = store.JobCancelled
		if err := s.Jobs().Update(ctx, jb); err != nil {
			t.Fatal(err)
		}

		bySession, err := s.Jobs().List(ctx, "org", store.JobFilter{SessionID: a.ID})
		if err != nil || len(bySession) != 1 || bySession[0].ID != ja.ID {
			t.Errorf("filter by session = %+v, %v", bySession, err)
		}
		byState, err := s.Jobs().List(ctx, "org", store.JobFilter{States: []store.JobState{store.JobCancelled}})
		if err != nil || len(byState) != 1 || byState[0].ID != jb.ID {
			t.Errorf("filter by state = %+v, %v", byState, err)
		}
		// ListLive is the supervisor's restart query and has no org.
		live, err := s.Jobs().ListLive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, j := range live {
			got[j.ID] = true
		}
		if len(live) != 2 || !got[ja.ID] || !got[jo.ID] {
			t.Errorf("ListLive = %+v, want the two unfinished jobs across both orgs", live)
		}
	},
	// The two quotas do not overlap. A job's session is an ordinary
	// session row, so without this a queue of jobs would consume the
	// session limit too and lock its owner out of `spov fly` — the
	// interference the separate limit exists to prevent.
	//
	// @decision(DL-045)
	"jobs: a job's session does not count against the session quota": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		plain := runningSession(t, s, "org", "u1", "plain")
		owned := runningSession(t, s, "org", "u1", "owned")
		_ = plain

		// Both sessions are live, so both count until one gains a job.
		before, err := s.Sessions().CountLive(ctx, "org", "u1")
		if err != nil {
			t.Fatal(err)
		}
		if before != 2 {
			t.Fatalf("CountLive = %d before any job exists, want 2", before)
		}

		j := job(store.NewID(t0), "org", "u1", owned.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		after, err := s.Sessions().CountLive(ctx, "org", "u1")
		if err != nil {
			t.Fatal(err)
		}
		if after != 1 {
			t.Errorf("CountLive = %d once a job owns one of the sessions, want 1", after)
		}
		// And it is counted by the job quota instead, not by neither.
		jobs, err := s.Jobs().CountLive(ctx, "org", "u1")
		if err != nil {
			t.Fatal(err)
		}
		if jobs != 1 {
			t.Errorf("Jobs().CountLive = %d, want 1: the job escaped both limits", jobs)
		}

		// A finished job releases its job slot. Its session may still be
		// running — a terminate that failed leaves a billing sandbox —
		// so the session must go back to counting, or that sandbox is
		// counted by neither limit and its owner can strand another.
		for _, st := range pathToJobState(t, store.JobCancelled) {
			j.State = st
			if err := s.Jobs().Update(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		if jobs, err = s.Jobs().CountLive(ctx, "org", "u1"); err != nil || jobs != 0 {
			t.Errorf("Jobs().CountLive = %d, %v after the job was cancelled", jobs, err)
		}
		back, err := s.Sessions().CountLive(ctx, "org", "u1")
		if err != nil {
			t.Fatal(err)
		}
		if back != 2 {
			t.Errorf("CountLive = %d once the job is terminal, want 2:"+
				" its still-running session is counted by neither quota", back)
		}
	},
	// The job quota's count. Every non-terminal state counts, `queued`
	// included — which is where it differs from the session count, and
	// the difference that stops a caller queueing a thousand jobs
	// against a limit of three.
	//
	// @decision(DL-045)
	"jobs: the live count is per owner, per org, and counts queued": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		n := 0
		// One session each: a session may hold only one job.
		create := func(org, owner string, state store.JobState) {
			t.Helper()
			n++
			sess := runningSession(t, s, org, owner, fmt.Sprintf("s%d", n))
			j := job(store.NewID(t0.Add(time.Duration(n)*time.Second)), org, owner, sess.ID, store.JobQueued)
			if err := s.Jobs().Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			if state == store.JobQueued {
				return
			}
			// Walked rather than written directly: the machine refuses
			// a jump, so this is also a check that the states named
			// here are reachable ones.
			for _, step := range pathToJobState(t, state) {
				j.State = step
				if err := s.Jobs().Update(ctx, j); err != nil {
					t.Fatalf("moving to %s: %v", step, err)
				}
			}
		}
		create("org", "u1", store.JobQueued)
		create("org", "u1", store.JobRunning)
		create("org", "u1", store.JobBlocked)
		create("org", "u1", store.JobSteered)
		create("org", "u1", store.JobCancelled)
		create("org", "u1", store.JobSucceeded)
		create("org", "u2", store.JobRunning)
		create("other", "u1", store.JobRunning)

		// Four unfinished: queued, running, blocked, steered. The
		// cancelled and succeeded ones have freed their slots.
		got, err := s.Jobs().CountLive(ctx, "org", "u1")
		if err != nil {
			t.Fatal(err)
		}
		if got != 4 {
			t.Errorf("CountLive = %d, want 4 (queued, running, blocked, steered)", got)
		}
		if got, err = s.Jobs().CountLive(ctx, "org", "u2"); err != nil || got != 1 {
			t.Errorf("CountLive for u2 = %d, %v", got, err)
		}
		// Another org's jobs are not this one's, even for the same user.
		if got, err = s.Jobs().CountLive(ctx, "other", "u1"); err != nil || got != 1 {
			t.Errorf("CountLive in the other org = %d, %v", got, err)
		}
		if got, err = s.Jobs().CountLive(ctx, "org", "nobody"); err != nil || got != 0 {
			t.Errorf("CountLive for an unknown owner = %d, %v", got, err)
		}
	},
	"jobs: the supervisor's cursor round-trips and survives an update": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		// A new job has read nothing.
		got, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Cursor != 0 {
			t.Errorf("a new job starts at cursor %d, want 0", got.Cursor)
		}

		// The cursor is what stops the supervisor re-deriving state
		// from events it has already read, so it has to persist.
		got.Cursor = 4242
		got.State = store.JobProvisioning
		if err := s.Jobs().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		back, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if back.Cursor != 4242 {
			t.Errorf("cursor = %d after update, want 4242", back.Cursor)
		}

		// An update that does not mention the cursor must not reset it.
		back.State = store.JobRunning
		if err := s.Jobs().Update(ctx, back); err != nil {
			t.Fatal(err)
		}
		last, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if last.Cursor != 4242 {
			t.Errorf("cursor = %d after a later update, want it kept at 4242", last.Cursor)
		}
	},
	"jobs: an invalid job is refused by both writes": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		base := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)

		bad := map[string]func(j store.Job) store.Job{
			"no goal":          func(j store.Job) store.Job { j.Goal = "  "; return j },
			"no session":       func(j store.Job) store.Job { j.SessionID = ""; return j },
			"unknown kind":     func(j store.Job) store.Job { j.Kind = "sculpture"; return j },
			"bad state":        func(j store.Job) store.Job { j.State = "loitering"; return j },
			"negative attempt": func(j store.Job) store.Job { j.Attempt = -1; return j },
		}
		for name, mangle := range bad {
			if err := s.Jobs().Create(ctx, mangle(base)); !errors.Is(err, store.ErrInvalidJob) {
				t.Errorf("Create with %s = %v, want ErrInvalidJob", name, err)
			}
		}
		if err := s.Jobs().Create(ctx, base); err != nil {
			t.Fatal(err)
		}
		for name, mangle := range bad {
			if err := s.Jobs().Update(ctx, mangle(base)); !errors.Is(err, store.ErrInvalidJob) {
				t.Errorf("Update with %s = %v, want ErrInvalidJob", name, err)
			}
		}
	},
	// Every text and boolean field a job carries, through a create and
	// through an update.
	//
	// A wrong sqlc parameter ORDER is invisible to the memory store and
	// to every unit test: the types line up, so the columns silently
	// swap. Only a round trip against real Postgres catches it, and
	// a field added later renumbers the same insert.
	//
	// @decision(DL-047) @decision(DL-050)
	// @decision(DL-051)
	"jobs: a deliverable is improved and never made worse": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobRunning)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		pr := store.Deliverable{
			JobID: j.ID, Key: store.PullRequestKey("codefloe", "acme", "app", "fix"),
			Kind: store.DeliverablePR, Status: store.DeliverableUnverified, Detail: "could not ask",
			Platform: "codefloe", Forge: "forgejo", Owner: "acme", Name: "app", Branch: "fix",
		}
		put := func(d store.Deliverable) bool {
			t.Helper()
			wrote, err := s.Jobs().PutDeliverable(ctx, "org", d)
			if err != nil {
				t.Fatal(err)
			}
			return wrote
		}
		if !put(pr) {
			t.Fatal("a new member was not written")
		}
		// Gate 2's write carries no forge; the recorded one is kept.
		// @decision(DL-051)
		pr.Status, pr.Detail, pr.Number, pr.URL, pr.Title, pr.Forge = store.DeliverableVerified, "", 7, "https://x/pulls/7", "Fix it", ""
		if !put(pr) {
			t.Fatal("an unverified member was not improved to verified")
		}
		worse := pr
		worse.Status, worse.Detail, worse.URL = store.DeliverableUnverified, "broke", ""
		if put(worse) {
			t.Error("a verified member was replaced by a later write")
		}
		report := store.Deliverable{
			JobID: j.ID, Key: store.ReportKey("a.md"), Kind: store.DeliverableReport,
			Status: store.DeliverableVerified, Title: "A", Path: "a.md", Size: 12, ImagesLost: true,
		}
		put(report)
		got, err := s.Jobs().Deliverables(ctx, "org", []string{j.ID, "nope"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || len(got[j.ID]) != 2 {
			t.Fatalf("deliverables = %+v", got)
		}
		g := got[j.ID][0]
		if g.Key != pr.Key || g.Status != store.DeliverableVerified || g.Number != 7 ||
			g.URL != pr.URL || g.Title != "Fix it" || g.Branch != "fix" || g.Owner != "acme" || g.Forge != "forgejo" ||
			g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() {
			t.Errorf("the pull request member = %+v", g)
		}
		// @decision(DL-051)
		if r := got[j.ID][1]; r.Path != "a.md" || r.Size != 12 || r.Title != "A" || r.Kind != store.DeliverableReport || !r.ImagesLost {
			t.Errorf("the report member = %+v", r)
		}
		if other, _ := s.Jobs().Deliverables(ctx, "other", []string{j.ID}); len(other) != 0 {
			t.Errorf("another org read the inventory: %+v", other)
		}
		if _, err := s.Jobs().PutDeliverable(ctx, "other", report); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org wrote a member: %v", err)
		}
		// @decision(DL-051)
		gone := pr
		gone.Key, gone.Branch, gone.Status, gone.Detail = store.PullRequestKey("codefloe", "acme", "app", "gone"), "gone",
			store.DeliverableWithdrawn, "withdrawn at the reader's request"
		put(gone)
		got, _ = s.Jobs().Deliverables(ctx, "org", []string{j.ID})
		found := false
		for _, d := range got[j.ID] {
			found = found || (d.Key == gone.Key && d.Status == store.DeliverableWithdrawn)
		}
		if !found {
			t.Errorf("a withdrawn member did not round-trip: %+v", got[j.ID])
		}
	},

	// @decision(DL-051)
	"jobs: a collected file is stored once": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobRunning)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		body := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xff}
		f := store.JobFile{JobID: j.ID, Path: "images/a.png", ContentType: "image/png", Size: int64(len(body)), Body: body}
		if wrote, err := s.Jobs().PutJobFile(ctx, "org", f); err != nil || !wrote {
			t.Fatalf("put = %v, %v", wrote, err)
		}
		again := f
		again.Body = []byte("replaced")
		if wrote, err := s.Jobs().PutJobFile(ctx, "org", again); err != nil || wrote {
			t.Errorf("a second put = %v, %v; want kept", wrote, err)
		}
		got, err := s.Jobs().JobFile(ctx, "org", j.ID, "images/a.png")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Body) != string(body) || got.ContentType != "image/png" || got.Size != int64(len(body)) {
			t.Errorf("the file = %+v", got)
		}
		if _, err := s.Jobs().PutJobFile(ctx, "org", store.JobFile{JobID: j.ID, Path: "empty.md", ContentType: "text/markdown"}); err != nil {
			t.Errorf("an empty file: %v", err)
		}
		list, err := s.Jobs().JobFiles(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || list[0].Path != "empty.md" || list[1].Path != "images/a.png" || list[1].Body != nil || list[1].Size != int64(len(body)) {
			t.Errorf("the listing = %+v", list)
		}
		if _, err := s.Jobs().JobFile(ctx, "org", j.ID, "missing.md"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a missing file = %v", err)
		}
		if _, err := s.Jobs().JobFile(ctx, "other", j.ID, "images/a.png"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org read the file: %v", err)
		}
		if _, err := s.Jobs().JobFiles(ctx, "other", j.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another org listed the files: %v", err)
		}
	},

	"jobs: every field of a job survives a round trip": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		// EVERY field a job carries, not the ones a job happens to be
		// created with. A column that reaches UpdateJob but not this
		// insert defaults to a zero value, so a Create that dropped it
		// did so silently. A test named "every field" that sets only some
		// would let that pass.
		j.Goal = "the goal"
		j.Unfinished = "the migration"
		j.Error = "something to say"
		j.Attempt = 3
		j.Cursor = 4242
		j.CheckCommand = "./scripts/check.sh"
		j.CheckProposed = true
		j.Verdict = "check: passed"
		j.Assessment = "spov's reading of the goal"
		j.Model = "anthropic/claude-sonnet-5"
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []struct {
			name      string
			got, want any
		}{
			{"Goal", got.Goal, j.Goal},
			{"Unfinished", got.Unfinished, j.Unfinished},
			{"Error", got.Error, j.Error},
			{"Attempt", got.Attempt, j.Attempt},
			{"Cursor", got.Cursor, j.Cursor},
			{"CheckCommand", got.CheckCommand, j.CheckCommand},
			{"CheckProposed", got.CheckProposed, j.CheckProposed},
			{"Verdict", got.Verdict, j.Verdict},
			{"Assessment", got.Assessment, j.Assessment},
			{"Model", got.Model, j.Model},
		} {
			if f.got != f.want {
				t.Errorf("a created job lost %s: got %v, want %v", f.name, f.got, f.want)
			}
		}

		// And through an update, which is the path the preparation pass
		// and the completion tool both take.
		got.Assessment = "a second reading"
		got.CheckCommand = "go test ./..."
		got.CheckProposed = false
		got.Verdict = "check: passed"
		got.Unfinished = "the docs"
		if err := s.Jobs().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		back, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if back.Assessment != "a second reading" || back.CheckCommand != "go test ./..." ||
			back.CheckProposed ||
			back.Verdict != "check: passed" || back.Unfinished != "the docs" {
			t.Errorf("an updated job lost a field: %+v", back)
		}
		// The model is written once, at create, and DL-014 leaves
		// changing a running job's model out of scope. UpdateJob does
		// not set the column, so the store must carry it over rather
		// than take what the caller passed — asserted by passing a
		// BLANK one, since an update built from a fresh read carries
		// the model along and would pass either way.
		blanked := back
		blanked.Model = ""
		if err := s.Jobs().Update(ctx, blanked); err != nil {
			t.Fatal(err)
		}
		back, err = s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if back.Model != j.Model {
			t.Errorf("an update blanked the model: got %q, want %q", back.Model, j.Model)
		}
	},
	"jobs: a job needs a session of its own org": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// The composite foreign key's rule, and the reason it is one:
		// a job filed under an org that cannot see the session it
		// names would be listed in the wrong place.
		orphan := job(store.NewID(t0), "org", "u1", "no-such-session", store.JobQueued)
		if err := s.Jobs().Create(ctx, orphan); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a job for a session that does not exist = %v, want ErrNotFound", err)
		}
		sess := runningSession(t, s, "other", "u2", "s1")
		crossOrg := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, crossOrg); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a job naming another org's session = %v, want ErrNotFound", err)
		}
	},
	"jobs: an update cannot move a job between sessions or change its kind": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		a := runningSession(t, s, "org", "u1", "s1")
		b := runningSession(t, s, "org", "u1", "s2")
		j := job(store.NewID(t0), "org", "u1", a.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		// Re-pointing a job at another session would slip past the
		// one-job-per-session rule that Create and the unique
		// constraint both defend; changing its kind would change what
		// gate 2 verifies and which credentials it was narrowed to.
		moved := j
		moved.SessionID = b.ID
		moved.Kind = store.JobKindReport
		moved.OwnerID = "someone-else"
		moved.State = store.JobProvisioning
		if err := s.Jobs().Update(ctx, moved); err != nil {
			t.Fatal(err)
		}
		got, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SessionID != a.ID {
			t.Errorf("the job moved to session %s", got.SessionID)
		}
		if got.Kind != store.JobKindPR {
			t.Errorf("the job's kind changed to %s", got.Kind)
		}
		if got.OwnerID != "u1" {
			t.Errorf("the job's owner changed to %s", got.OwnerID)
		}
		// The state, which Update does write, moved.
		if got.State != store.JobProvisioning {
			t.Errorf("the state did not move: %s", got.State)
		}
		// And the session it was always on still finds it.
		if bySess, err := s.Jobs().BySession(ctx, "org", a.ID); err != nil || bySess.ID != j.ID {
			t.Errorf("BySession(original) = %+v, %v", bySess, err)
		}
	},
	"jobs: a returned job does not alias the stored one": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		j := job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)
		if err := s.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		for _, to := range []store.JobState{store.JobProvisioning, store.JobRunning} {
			j.State = to
			if err := s.Jobs().Update(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil || got.StartedAt == nil {
			t.Fatalf("job = %+v, %v", got, err)
		}
		want := *got.StartedAt
		// A caller writing through the pointer it was handed must not
		// reach the stored row.
		*got.StartedAt = t0.Add(-24 * time.Hour)
		again, err := s.Jobs().Get(ctx, "org", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !again.StartedAt.Equal(want) {
			t.Errorf("StartedAt became %v: the store handed out a pointer into its own row", again.StartedAt)
		}
	},
	"jobs: a rolled-back transaction leaves no job": func(t *testing.T, s store.Store) {
		ctx := context.Background()
		sess := runningSession(t, s, "org", "u1", "s1")
		boom := errors.New("boom")
		err := s.Tx(ctx, func(tx store.Store) error {
			if err := tx.Jobs().Create(ctx, job(store.NewID(t0), "org", "u1", sess.ID, store.JobQueued)); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if list, _ := s.Jobs().List(ctx, "org", store.JobFilter{}); len(list) != 0 {
			t.Errorf("a rolled-back job is visible: %+v", list)
		}
	},
}

// runningSession creates a running session for conversation rows to
// hang off, since they reference one.
func runningSession(t *testing.T, s store.Store, org, owner, name string) store.Session {
	t.Helper()
	sess := session(store.NewID(t0), org, owner, name, store.StateRunning)
	if err := s.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func conversation(t *testing.T, s store.Store, sess store.Session, opencodeID string) store.Conversation {
	t.Helper()
	conv, err := s.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(t0), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: opencodeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

// job builds a queued job for one session. The goal is what makes it
// valid, so a case that wants an invalid one blanks it deliberately.
func job(id, org, owner, sessionID string, state store.JobState) store.Job {
	return store.Job{
		ID: id, OrgID: org, OwnerID: owner, SessionID: sessionID,
		Kind: store.JobKindPR, State: state,
		Goal: "fix the failing test in internal/cli",
	}
}

// pathToJobState is the sequence of transitions from queued to want.
// The machine refuses a jump, so a test that wants a job in a
// particular state has to walk there.
//
// @decision(DL-045)
func pathToJobState(t *testing.T, want store.JobState) []store.JobState {
	t.Helper()
	switch want {
	case store.JobCancelled:
		return []store.JobState{store.JobCancelled}
	case store.JobProvisioning:
		return []store.JobState{store.JobProvisioning}
	case store.JobRunning:
		return []store.JobState{store.JobProvisioning, store.JobRunning}
	case store.JobBlocked, store.JobSteered:
		return []store.JobState{store.JobProvisioning, store.JobRunning, want}
	case store.JobDelivering:
		return []store.JobState{store.JobProvisioning, store.JobRunning, store.JobDelivering}
	case store.JobSucceeded, store.JobFailed:
		return []store.JobState{store.JobProvisioning, store.JobRunning, store.JobDelivering, want}
	}
	t.Fatalf("no path to %s", want)
	return nil
}

func ids(ss []store.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}
