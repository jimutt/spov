//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
)

// Spike S-MP1: how the pinned OpenCode pages a conversation's messages,
// which is what Recorder.readMessages relies on. It fills one
// conversation with turns against the fake model and checks the default
// page, the bounds on limit, the order, where cursors lead and where a
// message with a back-dated id sorts. No Modal, no key.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run SMP1 -v
//
// @decision(DL-069)
func TestIntegrationSMP1MessagePaging(t *testing.T) {
	if os.Getenv("SPOV_PROBE") == "" && os.Getenv("SPOV_PROBE_OPENCODE") == "" {
		t.Skip("set SPOV_PROBE=1 to run the spike")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	model := newFakeModel(t)
	work := t.TempDir()
	conn := startProbeServerWith(ctx, t, probeBinary(ctx, t), model.URL, work, "")
	if _, err := opencode.NewClient(conn.URL, conn.Password).WaitHealthy(ctx, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	id, err := createConversation(ctx, conn, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConversationModel(ctx, conn, id, "probe/m"); err != nil {
		t.Fatal(err)
	}
	p := &smp1{spikeRun: &spikeRun{t: t, ctx: ctx, conn: conn, id: id, start: time.Now()}}

	// Seventy turns: a user, an assistant and an idle message each.
	const turns = 70
	for i := range turns {
		before := len(p.all("asc"))
		p.prompt(fmt.Sprintf("turn %d", i), "steer", true)
		deadline := time.Now().Add(30 * time.Second)
		for len(p.all("asc")) < before+2 {
			if time.Now().After(deadline) {
				t.Fatalf("turn %d never answered", i)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	all := p.all("asc")
	t.Logf("%d messages after %d turns", len(all), turns)
	for i := 1; i < len(all); i++ {
		if all[i-1].Time.Created > all[i].Time.Created {
			t.Errorf("not ordered by time.created at %d: %s then %s", i, all[i-1].ID, all[i].ID)
		}
	}

	// The unparameterised read the recorder used to make: the newest 50.
	code, bare, _ := p.page(nil)
	if code != http.StatusOK || len(bare.Data) != MessagePageSize || bare.Data[0].ID != all[len(all)-1].ID {
		t.Errorf("no parameters: %d, %d messages, first %v; want the newest %d", code, len(bare.Data), firstOf(bare.Data), MessagePageSize)
	}

	for limit, want := range map[string]int{"0": 400, "1": 200, "200": 200, "201": 400} {
		if code, _, out := p.page(url.Values{"limit": {limit}}); code != want {
			t.Errorf("limit=%s: %d, want %d: %s", limit, code, want, clip(out, 200))
		}
	}

	// Newest first, followed to the end, is the whole conversation; the
	// short last page still carries a cursor.
	desc := p.all("desc")
	if len(desc) != len(all) || desc[0].ID != all[len(all)-1].ID || desc[len(desc)-1].ID != all[0].ID {
		t.Errorf("newest first: %d messages from %s to %s", len(desc), firstOf(desc), lastOf(desc))
	}
	seen := map[string]bool{}
	for _, m := range desc {
		if seen[m.ID] {
			t.Errorf("%s is listed twice", m.ID)
		}
		seen[m.ID] = true
	}
	if p.lastNext == "" {
		t.Error("the short last page carries no next cursor; readMessages may stop on it instead")
	} else if code, past, out := p.page(url.Values{"cursor": {p.lastNext}}); code != http.StatusOK || len(past.Data) != 0 {
		t.Errorf("the cursor past the end: %d, %d messages: %s", code, len(past.Data), clip(out, 200))
	}

	if code, _, out := p.page(url.Values{"cursor": {"not-a-cursor"}}); code != http.StatusBadRequest {
		t.Errorf("a forged cursor: %d %s", code, clip(out, 200))
	}
	if bare.Cursor.Next != nil {
		if code, _, out := p.page(url.Values{"cursor": {*bare.Cursor.Next}, "order": {"desc"}}); code != http.StatusBadRequest {
			t.Errorf("a cursor with an order: %d %s", code, clip(out, 200))
		}
	}

	// An id stamped ten minutes back, as a browser with a slow clock
	// mints one, still sorts as the newest message.
	skewed := fmt.Sprintf("%s%012x%s", MessageIDPrefix, time.Now().Add(-10*time.Minute).UnixMilli(), "skewed000001")
	if code, out := p.do(http.MethodPost, "/prompt", map[string]any{"id": skewed, "text": "skewed", "resume": true, "delivery": "steer"}); code != http.StatusOK {
		t.Fatalf("the back-dated prompt: %d %s", code, clip(string(out), 200))
	}
	time.Sleep(3 * time.Second)
	found := false
	for i, m := range p.all("asc") {
		if m.ID == skewed {
			found = true
			if i < len(all) {
				t.Errorf("the back-dated message sorts at %d, among the %d messages before it", i, len(all))
			}
		}
	}
	if !found {
		t.Error("the back-dated message was never listed")
	}
}

type smp1Message struct {
	ID   string `json:"id"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type smp1Page struct {
	Data   []smp1Message `json:"data"`
	Cursor struct {
		Next *string `json:"next"`
	} `json:"cursor"`
}

type smp1 struct {
	*spikeRun
	lastNext string
}

func (p *smp1) page(query url.Values) (int, smp1Page, string) {
	path := "/message"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	code, out := p.do(http.MethodGet, path, nil)
	var page smp1Page
	_ = json.Unmarshal(out, &page)
	return code, page, string(out)
}

// all follows next from a first page in order until a short page.
func (p *smp1) all(order string) []smp1Message {
	query := url.Values{"limit": {"200"}, "order": {order}}
	var out []smp1Message
	for range 100 {
		code, page, body := p.page(query)
		if code != http.StatusOK {
			p.t.Fatalf("reading the messages: %d %s", code, clip(body, 300))
		}
		out = append(out, page.Data...)
		p.lastNext = ""
		if page.Cursor.Next != nil {
			p.lastNext = *page.Cursor.Next
		}
		if len(page.Data) < 200 || p.lastNext == "" {
			break
		}
		query = url.Values{"limit": {"200"}, "cursor": {p.lastNext}}
	}
	return out
}

func firstOf(m []smp1Message) string {
	if len(m) == 0 {
		return ""
	}
	return m[0].ID
}

func lastOf(m []smp1Message) string {
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1].ID
}
