//go:build integration

package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// TestIntegrationMeasureLongConversations serves the real web app over a
// store seeded with long sessions, for a browser to measure. The sessions
// are copies of one exported real session, its ids renamed per copy. It
// holds the server open for SPOV_MEASURE_HOLD and appends one event to
// each session every two seconds, as a live tail would, unless a file
// named quiet is in the export directory; a file named stop ends it.
//
// The export is the session's rows as JSON arrays: conversations.json
// (id, opencode_id), messages.json and events.json (conversation_id,
// opencode_id, source, seq, name, role, text, at, raw), from psql's
// json_agg over the three tables.
//
//	SPOV_MEASURE_DIR=<export> SPOV_MEASURE_COPIES=13,65,130 \
//	  go test -tags integration ./internal/server/ -run MeasureLong -v -timeout 0
//
// @decision(DL-070)
func TestIntegrationMeasureLongConversations(t *testing.T) {
	dir := os.Getenv("SPOV_MEASURE_DIR")
	if dir == "" {
		t.Skip("set SPOV_MEASURE_DIR to an exported session")
	}
	hold, err := time.ParseDuration(envOr("SPOV_MEASURE_HOLD", "30m"))
	if err != nil {
		t.Fatal(err)
	}
	web := envOr("SPOV_MEASURE_WEB", "../../web/dist")
	ctx := context.Background()
	// The memory store by default, as a local server; Postgres, as a
	// hosted one, when SPOV_MEASURE_DATABASE_URL names a migrated database.
	// Use a throwaway one: the run leaves its user, its running sessions
	// with made-up sandbox ids and their rows behind.
	var st store.Store = store.NewMemory()
	who := auth.Local()
	if url := os.Getenv("SPOV_MEASURE_DATABASE_URL"); url != "" {
		pg, err := postgres.Open(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pg.Close)
		st = pg
		who = auth.Principal{UserID: store.NewID(time.Now()), OrgID: "default"}
		if err := st.Users().Create(ctx, store.User{ID: who.UserID, OrgID: who.OrgID, Email: who.UserID + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Store = st
		d.Auth = auth.StaticAuthenticator{Principal: who}
		d.WebUI = os.DirFS(web)
	})
	exp := loadExport(t, dir)
	var live []*measured
	for _, s := range strings.Split(envOr("SPOV_MEASURE_COPIES", "13,65,130"), ",") {
		copies, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		m := seedLong(ctx, t, st, exp, copies, who)
		t.Logf("seeded %s: %d messages, %d events in %s", m.sess.Name, m.messages, m.events, time.Since(start).Round(time.Millisecond))
		live = append(live, m)
	}
	t.Logf("serving %s", f.http.URL)
	for _, m := range live {
		t.Logf("  %s/session/?id=%s", f.http.URL, m.sess.ID)
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	deadline := time.After(hold)
	for {
		select {
		case <-deadline:
			return
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				return
			}
			// A quiet file pauses the appends, so a page can be timed
			// opening with nothing arriving.
			if _, err := os.Stat(filepath.Join(dir, "quiet")); err == nil {
				continue
			}
			for _, m := range live {
				m.appendLive(ctx, t, st)
			}
		}
	}
}

type exportRow struct {
	ConversationID string  `json:"conversation_id"`
	OpenCodeID     string  `json:"opencode_id"`
	Source         string  `json:"source"`
	Seq            *int64  `json:"seq"`
	Name           string  `json:"name"`
	Role           string  `json:"role"`
	Text           string  `json:"text"`
	At             string  `json:"at"`
	Raw            *string `json:"raw"`
}

type export struct {
	convs    []struct{ ID, OpenCodeID string }
	messages []exportRow
	events   []exportRow
}

func loadExport(t *testing.T, dir string) export {
	t.Helper()
	var e export
	read := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var convs []struct {
		ID         string `json:"id"`
		OpenCodeID string `json:"opencode_id"`
	}
	read("conversations.json", &convs)
	for _, c := range convs {
		e.convs = append(e.convs, struct{ ID, OpenCodeID string }{c.ID, c.OpenCodeID})
	}
	read("messages.json", &e.messages)
	read("events.json", &e.events)
	return e
}

func parseAt(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse("2006-01-02T15:04:05.999999999-07:00", s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return at.UTC()
}

func rawOf(t *testing.T, r *string) []byte {
	t.Helper()
	if r == nil {
		return nil
	}
	if strings.HasPrefix(*r, `\x`) {
		b, err := hex.DecodeString((*r)[2:])
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return []byte(*r)
}

var copyIDs = regexp.MustCompile(`"((?:evt|msg|call|rs)_[0-9A-Za-z]{6,})"`)

func renamed(raw []byte, k int) []byte {
	return copyIDs.ReplaceAll(raw, []byte(fmt.Sprintf(`"${1}z%03d"`, k)))
}

type measured struct {
	sess     store.Session
	conv     string
	ocid     string
	seq      int64
	at       time.Time
	messages int
	events   int
}

// seedLong writes copies of the export, one after another in time, into
// a new running session, in one batch per copy.
func seedLong(ctx context.Context, t *testing.T, st store.Store, e export, copies int, who auth.Principal) *measured {
	t.Helper()
	now := time.Now()
	sess := store.Session{
		ID: store.NewID(now), OrgID: who.OrgID, OwnerID: who.UserID,
		Name: fmt.Sprintf("long-%dk", copies*len(e.messages)/1000), State: store.StateRunning, SandboxID: "sb-" + store.NewID(now),
		CreatedAt: now.Add(-time.Hour),
	}
	if err := st.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	convs := map[string]string{}
	m := &measured{sess: sess}
	for _, c := range e.convs {
		conv, err := st.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(time.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: c.OpenCodeID,
		})
		if err != nil {
			t.Fatal(err)
		}
		convs[c.ID] = conv.ID
		m.conv, m.ocid = conv.ID, c.OpenCodeID
	}
	first := parseAt(t, e.events[0].At)
	last := parseAt(t, e.events[len(e.events)-1].At)
	span := last.Sub(first) + time.Minute
	var maxSeq int64
	for _, ev := range e.events {
		if ev.Seq != nil && *ev.Seq > maxSeq {
			maxSeq = *ev.Seq
		}
	}
	// The copies end now, as a session still running would.
	base := now.Add(-time.Duration(copies) * span).Sub(first)
	for k := range copies {
		shift := base + time.Duration(k)*span
		var msgs []store.Message
		for _, r := range e.messages {
			msgs = append(msgs, store.Message{
				ID: store.NewID(time.Now()), OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: convs[r.ConversationID],
				OpenCodeID: fmt.Sprintf("%sz%03d", r.OpenCodeID, k), Role: r.Role, Text: r.Text,
				At: parseAt(t, r.At).Add(shift), Raw: renamed(rawOf(t, r.Raw), k),
			})
		}
		if _, err := st.Conversations().PutMessages(ctx, msgs); err != nil {
			t.Fatal(err)
		}
		var evs []store.Event
		for _, r := range e.events {
			ev := store.Event{
				ID: store.NewID(time.Now()), OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: convs[r.ConversationID],
				Source: store.EventSource(r.Source), Name: r.Name, Text: r.Text,
				At: parseAt(t, r.At).Add(shift), Raw: renamed(rawOf(t, r.Raw), k),
			}
			if r.Seq != nil {
				ev.Seq = *r.Seq + int64(k)*(maxSeq+1)
			}
			evs = append(evs, ev)
		}
		if _, err := st.Conversations().PutEvents(ctx, evs); err != nil {
			t.Fatal(err)
		}
		m.messages += len(msgs)
		m.events += len(evs)
	}
	m.seq = int64(copies) * (maxSeq + 1)
	m.at = now
	return m
}

// appendLive adds one finished text part, as a turn writing would.
func (m *measured) appendLive(ctx context.Context, t *testing.T, st store.Store) {
	m.seq++
	m.at = time.Now()
	raw := fmt.Sprintf(`{"id":"evt_live%d","created":%d,"type":"session.text.ended","data":{"sessionID":%q,"text":"live line %d"},"durable":{"aggregateID":%q,"seq":%d,"version":1}}`,
		m.seq, m.at.UnixMilli(), m.ocid, m.seq, m.ocid, m.seq)
	_, err := st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(m.at), OrgID: m.sess.OrgID, SessionID: m.sess.ID, ConversationID: m.conv,
		Source: store.SourceOpenCode, Seq: m.seq, Name: "session.text.ended", Text: fmt.Sprintf("live line %d", m.seq), At: m.at, Raw: []byte(raw),
	}})
	if err != nil {
		t.Errorf("live append: %v", err)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
