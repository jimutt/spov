//go:build integration

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// These are the live-steering spikes, run against the deployment's OpenCode
// pin: S-LS1 (TestIntegrationSLS1SteeringSurface) reads the steering surface,
// S-LS2 (TestIntegrationSLS2InterruptedToolPart) the tool part an interrupt
// leaves, and S-LS3 (TestIntegrationSLS3InterruptVersusReject) an interrupt
// beside a human reject in one session. They record no decision; their results
// (2026-09-14/15, 0.0.0-beta-19151) are in DL-019, DL-053 and DL-059. Each
// creates one billable sandbox and terminates it. S-LS2 and S-LS3 need a
// working model in the image.
//
// The steering design was read at 0.0.0-beta-19378, and a detail that drifts
// fails silently in production, so re-run these on any OpenCode version bump:
//
//	go test -tags integration ./internal/sandbox/ -run SLS -v
//
// @decision(DL-019) @decision(DL-052) @decision(DL-053) @decision(DL-059)

// steerPinnedVersion is the deployment's pin, read through the contract
// because internal/config imports this package (DL-035).
func steerPinnedVersion() string { return contract.Version() }

// steerRequirePin refuses to run against anything but the deployment's
// pin unless the caller asked for another build by name.
func steerRequirePin(t *testing.T) {
	t.Helper()
	got := testOpenCodeVersion()
	// SPOV_TEST_IMAGE overrides the image independently of the version,
	// so a matching version string does not mean a matching build.
	if want := (ImageSpec{OpenCodeVersion: got}).Name(); testImage() != want {
		t.Fatalf("SPOV_TEST_IMAGE is %s, which is not the image for %s (%s)."+
			" The version guard cannot vouch for a build it did not name.",
			testImage(), got, want)
	}
	// The default build is the pin; only SPOV_TEST_OPENCODE_VERSION
	// names another.
	if got != steerPinnedVersion() {
		t.Logf("NOT the deployment's pin: measuring %s, while spov ships %s."+
			" Findings from this run are about drift, not about what a sandbox does today.",
			got, steerPinnedVersion())
	}
}

// steerMarker is the shell command S-LS2's turn is interrupted during.
// It sleeps long enough that the interrupt lands inside the tool call
// rather than between two of them.
const steerMarker = "spov-sls2-interrupt-me"

// TestIntegrationSLS1SteeringSurface answers S-LS1: is the steering
// surface at the pin what the research read at 19378?
func TestIntegrationSLS1SteeringSurface(t *testing.T) {
	steerRequirePin(t)
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	_, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-sls1", 8*time.Minute, nil)
	// Timeout 0, as in every streaming test here: http.Client.Timeout also
	// bounds reading the body, so a finite one kills the held-open
	// /api/event stream. steerCall, get and post set per-request deadlines.
	client := &http.Client{Timeout: 0}
	base := creds.URL
	session := createSession(ctx, t, client, base, creds.Token, password)

	doc := steerOpenAPI(ctx, t, client, base, creds.Token, password)

	// Question 1: the routes. steerHasRoute confirms their shape, not their
	// parameter spellings; the three inbox item routes are checked against
	// the document only, since nothing here calls them.
	t.Run("the sandbox's own document declares the steering routes", func(t *testing.T) {
		if len(doc.Paths) == 0 {
			// Fatal, not a skip: decision records cite this result.
			t.Fatal("the sandbox's /openapi.json could not be read, so the route" +
				" table is unverified — DL-019 cites it")
		}
		want := map[string]string{
			"/session/{id}/prompt":    "DL-019's send",
			"/session/{id}/interrupt": "DL-053's stop",
			"/session/{id}/inbox":     "the queue listing",
			// GA moves an item between steer and queue with PATCH
			// {delivery} on the item, where the beta had /steer and
			// /queue routes; DELETE cancels it.
			"/session/{id}/inbox/{id}": "cancel, promote or demote an undelivered item",
		}
		declared := steerRoutes(doc)
		t.Logf("session routes mentioning prompt/interrupt/inbox: %v", declared)
		for shape, why := range want {
			if !steerHasRoute(declared, shape) {
				// DL-059 ships no queue view, so a missing inbox route
				// is an error rather than fatal; prompt and interrupt
				// are required.
				if strings.Contains(shape, "inbox") {
					t.Errorf("no route matching %s (%s): a later queue view has no endpoint", shape, why)
					continue
				}
				t.Fatalf("no route matching %s (%s): the steering batch has no endpoint at this build", shape, why)
			}
		}
	})

	// Question 2: the delivery vocabulary DL-052's rule for jobs is
	// written against.
	t.Run("delivery is the two-value union the research read", func(t *testing.T) {
		name, values := steerDeliveryEnum(t, doc)
		if name == "" {
			t.Skip("no schema in the document carries a `delivery` property with an enum")
		}
		t.Logf("%s.delivery = %v", name, values)
		for _, want := range []string{"steer", "queue"} {
			if !slices.Contains(values, want) {
				t.Errorf("the delivery enum %v has no %q: DL-052 forces that value on a job session", values, want)
			}
		}
	})

	// Questions 3 and 4, together: the id DL-059 mints in the browser,
	// and what comes back.
	t.Run("prompt accepts a client-minted msg_ id and dedupes a repeat", func(t *testing.T) {
		id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
		body := fmt.Sprintf(`{"id":%q,"text":"S-LS1: reply with the single word ack.","resume":false,"delivery":"steer"}`, id)

		code, first := steerCall(ctx, t, client, http.MethodPost, base+"/api/session/"+session+"/prompt",
			creds.Token, password, body)
		if code < 200 || code >= 300 {
			t.Fatalf("POST /prompt with a client-minted id -> %d (%s):"+
				" DL-059's echo key cannot be minted in the browser and the composer"+
				" has to round-trip before it can render anything",
				code, truncate(first, 300))
		}
		t.Logf("POST /prompt answered: %s", truncate(first, 600))
		if !strings.Contains(first, id) {
			t.Errorf("the answer does not carry the id that was sent (%s), so the caller"+
				" cannot confirm its own key was honoured: %s", id, truncate(first, 400))
		}

		// The retry DL-059 rests on. A 2xx alone does not prove a dedupe,
		// since a build that created a second item would answer 200 too,
		// so the returned record and the inbox count are compared as well.
		beforeRetry := steerInboxIDs(ctx, t, client, base, creds.Token, password, session)
		code2, second := steerCall(ctx, t, client, http.MethodPost, base+"/api/session/"+session+"/prompt",
			creds.Token, password, body)
		switch {
		case code2 >= 200 && code2 < 300:
			one, two := steerPromptRecordOf(t, first), steerPromptRecordOf(t, second)
			afterRetry := steerInboxIDs(ctx, t, client, base, creds.Token, password, session)
			switch {
			case one.ID != two.ID || one.Time.Created != two.Time.Created:
				t.Errorf("the repeat answered 200 but returned a DIFFERENT record"+
					" (id %q/%q, time.created %d/%d): the id is not an idempotency key and"+
					" a retry after a dropped response duplicates the reader's message",
					one.ID, two.ID, one.Time.Created, two.Time.Created)
			case len(afterRetry) != len(beforeRetry):
				t.Errorf("the repeat returned the original record but the inbox moved"+
					" from %d to %d item(s), so something was enqueued anyway",
					len(beforeRetry), len(afterRetry))
			default:
				t.Logf("confirmed: a repeated id returns the ORIGINAL record"+
					" (id %s, time.created %d, unchanged) and enqueues nothing further,"+
					" so DL-059's idempotency key is measured rather than assumed",
					one.ID, one.Time.Created)
			}
		case code2 == http.StatusConflict:
			t.Errorf("a repeated id in the SAME session answered 409: DL-059 records 409" +
				" as the cross-session answer only, and a retry after a dropped response" +
				" would surface as an error on a message that was in fact delivered")
		default:
			t.Errorf("a repeated id answered %d (%s): DL-059's idempotency key does not"+
				" dedupe, so the obvious response to a network error duplicates the message",
				code2, truncate(second, 300))
		}

		// The inbox and the message store must name this message with the
		// same id: storedMessage reads `inboxID` off the frame, parseMessages
		// reads `id` off /message, and the page compares the two (DL-059).
		if found, ok := steerMessageIDFor(ctx, t, client, base, creds.Token, password, session,
			"S-LS1: reply with the single word ack."); ok {
			if found != id {
				t.Errorf("the message store calls this message %q while the inbox calls it %q:"+
					" they are two id spaces, and DL-059's echo — and buildTimeline's"+
					" user-message identity — compare across them", found, id)
			} else {
				t.Logf("confirmed: the inbox and the message store agree on %s", id)
			}
		} else {
			t.Log("the message store has not taken this item yet (resume was false), so the" +
				" inbox-to-message id equality is untested on this run")
		}
	})

	// DL-019 and DL-052 rely on an omitted delivery resolving to `steer`.
	t.Run("an omitted delivery resolves to steer", func(t *testing.T) {
		body := fmt.Sprintf(`{"id":"msg_%d","text":"S-LS1: no delivery named.","resume":false}`,
			time.Now().UnixNano())
		code, raw := steerCall(ctx, t, client, http.MethodPost, base+"/api/session/"+session+"/prompt",
			creds.Token, password, body)
		if code < 200 || code >= 300 {
			t.Fatalf("a prompt with no delivery -> %d (%s)", code, truncate(raw, 300))
		}
		if got := steerPromptRecordOf(t, raw).Delivery; got != "steer" {
			t.Errorf("an omitted delivery resolved to %q, not \"steer\": DL-052 says a job"+
				" forces steer, which is only a statement of intent if steer is the default", got)
		} else {
			t.Log("confirmed: an omitted delivery resolves to steer")
		}
	})

	// Question 2b: both values are actually accepted on the wire, not
	// merely present in a schema.
	t.Run("both delivery values are accepted", func(t *testing.T) {
		for _, delivery := range []string{"steer", "queue"} {
			body := fmt.Sprintf(`{"id":"msg_%d","text":"S-LS1 %s.","resume":false,"delivery":%q}`,
				time.Now().UnixNano(), delivery, delivery)
			code, raw := steerCall(ctx, t, client, http.MethodPost, base+"/api/session/"+session+"/prompt",
				creds.Token, password, body)
			if code < 200 || code >= 300 {
				t.Errorf("delivery %q -> %d (%s)", delivery, code, truncate(raw, 300))
				continue
			}
			t.Logf("delivery %q accepted: %s", delivery, truncate(raw, 300))
		}
	})

	// Question 5: the interrupt rules DL-053 and DL-059 state. On its own
	// session, because the one above holds undelivered inbox items by now
	// and pending work changes what an interrupt does.
	t.Run("interrupt on an idle session", func(t *testing.T) {
		idle := createSession(ctx, t, client, base, creds.Token, password)
		u := base + "/api/session/" + idle + "/interrupt"

		// DL-053 sends resume=true explicitly.
		code, raw := steerCall(ctx, t, client, http.MethodPost, u+"?resume=true", creds.Token, password, "")
		if code < 200 || code >= 300 {
			t.Fatalf("POST /interrupt?resume=true -> %d (%s): DL-053 has no mechanism at this build",
				code, truncate(raw, 300))
		}
		t.Logf("interrupt?resume=true on an idle session -> %d: %s", code, truncate(raw, 300))

		// DL-059 renders `interrupted: false` as success.
		var out struct {
			Interrupted *bool `json:"interrupted"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Interrupted == nil {
			t.Errorf("the answer carries no `interrupted` boolean (%s): DL-059 keys its"+
				" success copy on that field", truncate(raw, 300))
		} else if *out.Interrupted {
			t.Errorf("an IDLE session answered interrupted:true, so the field does not" +
				" mean what the research read and DL-059's copy would be wrong")
		} else {
			t.Log("confirmed: an idle session answers interrupted:false rather than erroring")
		}

		code2, raw2 := steerCall(ctx, t, client, http.MethodPost, u, creds.Token, password, "")
		t.Logf("interrupt with NO resume on an idle session -> %d: %s", code2, truncate(raw2, 300))

		// `resume` is a string union ["true","false"], so resume=1 must
		// be a 400 rather than a truthy synonym.
		code3, raw3 := steerCall(ctx, t, client, http.MethodPost, u+"?resume=1", creds.Token, password, "")
		if code3 != http.StatusBadRequest {
			t.Errorf("resume=1 answered %d, not 400 (%s): DL-053 records the parameter"+
				" as a strict string union, and a client sending a truthy 1 would be"+
				" refused on the one control a reader presses when something is wrong",
				code3, truncate(raw3, 300))
		} else {
			// Decoded, not substring-matched: the quotes in the message
			// are JSON-escaped in the raw body.
			var refusal struct {
				Tag     string `json:"_tag"`
				Message string `json:"message"`
				Kind    string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(raw3), &refusal); err != nil {
				t.Errorf("the 400 body is not JSON (%v): %s", err, truncate(raw3, 300))
			} else if !strings.Contains(refusal.Message, `"true"`) || !strings.Contains(refusal.Message, `"false"`) {
				t.Errorf("resume=1 was refused but the reason does not name the two"+
					" values (%q), so a caller cannot tell a vocabulary error from"+
					" anything else", refusal.Message)
			} else {
				t.Logf("confirmed: resume=1 -> 400 %s, %q", refusal.Tag, refusal.Message)
			}
		}
	})

	// Does resume=true on an idle session deliver a waiting steered item
	// and start a turn (DL-053)? On its own session and event stream, with
	// two control arms before the treatment.
	t.Run("resume=true wakes a session that has steered input waiting", func(t *testing.T) {
		quiet := createSession(ctx, t, client, base, creds.Token, password)
		col := collectSandboxEvents(ctx, t, client, base, creds.Token, password)
		t.Cleanup(func() { t.Logf("stream: %v", col.report()) })

		// A turn started on this session: /api/event is the sandbox's
		// global stream and other sessions are live in this test.
		startedHere := func(f sseFrame) bool {
			return steerFrameName(f) == "session.execution.started" && steerFrameSession(f) == quiet
		}

		admit := func(text string) {
			t.Helper()
			// resume:false admits the item without scheduling the loop,
			// leaving the session idle with work waiting.
			body := fmt.Sprintf(`{"id":"msg_%d","text":%q,"resume":false,"delivery":"steer"}`,
				time.Now().UnixNano(), text)
			if code, raw := steerCall(ctx, t, client, http.MethodPost,
				base+"/api/session/"+quiet+"/prompt", creds.Token, password, body); code < 200 || code >= 300 {
				t.Fatalf("admitting the waiting item -> %d (%s)", code, truncate(raw, 300))
			}
		}

		// Every arm gets the same observation window: inbox delivery is
		// asynchronous, so a control read straight after its call could
		// miss a delivery that had not happened yet.
		const window = 45 * time.Second
		arm := func(name, query string) (started bool, before, after int) {
			t.Helper()
			admit("S-LS1: waiting to be steered in (" + name + ").")
			before = len(steerInboxIDs(ctx, t, client, base, creds.Token, password, quiet))
			code, raw := steerCall(ctx, t, client, http.MethodPost,
				base+"/api/session/"+quiet+"/interrupt"+query, creds.Token, password, "")
			t.Logf("interrupt%s with %d item(s) waiting -> %d: %s", query, before, code, truncate(raw, 300))
			if code < 200 || code >= 300 {
				t.Fatalf("interrupt%s -> %d", query, code)
			}
			// Every arm runs on an idle session, so a true means the
			// previous arm's turn was still going.
			var out struct {
				Interrupted *bool `json:"interrupted"`
			}
			if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Interrupted == nil {
				t.Fatalf("interrupt%s carries no `interrupted` boolean: %s", query, truncate(raw, 300))
			}
			if *out.Interrupted {
				t.Fatalf("interrupt%s answered interrupted:TRUE on a session that should be"+
					" idle, so this arm is measuring the previous arm's turn", query)
			}
			_, started = col.waitFor(ctx, window, startedHere)
			after = len(steerInboxIDs(ctx, t, client, base, creds.Token, password, quiet))
			return started, before, after
		}

		// The two controls first, and both must stay quiet: once either
		// wakes the session, startedHere stays true for the rest of the
		// subtest and the treatment's result is unreadable.
		if started, before, after := arm("no resume", ""); started || after != before {
			t.Fatalf("an ABSENT resume woke the session (started=%t, inbox %d -> %d),"+
				" so it does not behave as false and DL-053's note about the missing"+
				" server-side default is wrong", started, before, after)
		}
		t.Log("confirmed: an absent `resume` delivers nothing and starts nothing," +
			" so it behaves as false — which DL-053 had from source, not measurement")

		if started, before, after := arm("resume=false", "?resume=false"); started || after != before {
			t.Fatalf("resume=FALSE woke the session (started=%t, inbox %d -> %d), so the"+
				" parameter does not gate delivery and the treatment below can attribute"+
				" nothing to it", started, before, after)
		}
		t.Logf("confirmed: resume=false leaves the inbox at %d and starts nothing",
			len(steerInboxIDs(ctx, t, client, base, creds.Token, password, quiet)))

		// The treatment. Confirmation needs both halves: a turn started
		// and the waiting items delivered.
		started, before, after := arm("resume=true", "?resume=true")
		switch {
		case started && after < before:
			t.Logf("CONFIRMED: resume=true on an IDLE session started a turn AND emptied"+
				" the inbox (%d -> %d), while answering interrupted:false. So"+
				" interrupted:false does NOT mean nothing happened — Stop pressed on a"+
				" session with a steered message waiting DELIVERS it and begins work."+
				" DL-053 must send resume=true only where that is meant, and DL-059's"+
				" copy for interrupted:false cannot say \"the session was idle\".",
				before, after)
		case started:
			t.Errorf("resume=true started a turn but the inbox did not move (%d -> %d):"+
				" something woke the session that was not the waiting item, and the wake"+
				" cannot be attributed to the steered input", before, after)
		case after < before:
			t.Errorf("resume=true delivered the waiting item (%d -> %d) but no"+
				" session.execution.started arrived within %s: the wake is real and the"+
				" frame DL-059's Stop keys on did not follow it", before, after, window)
		default:
			t.Logf("resume=true did NOT wake the session: the inbox still holds %d"+
				" item(s) and no turn started within %s. The binary's \"resumes pending"+
				" steering input\" applies to an interrupted execution only, and DL-059's"+
				" interrupted:false copy is complete as written.", after, window)
		}
	})

	// The routes DL-059 does not use yet but a queue view would.
	t.Run("the inbox lists undelivered work", func(t *testing.T) {
		code, raw := steerCall(ctx, t, client, http.MethodGet, base+"/api/session/"+session+"/inbox",
			creds.Token, password, "")
		t.Logf("GET /inbox -> %d: %s", code, truncate(raw, 600))
		if code < 200 || code >= 300 {
			t.Errorf("GET /inbox -> %d: a queue view has no source, and DL-059's"+
				" store-side derivation becomes the only option rather than the likelier one", code)
		}
	})

	// Question 6: the gate on DL-059's Stop control.
	t.Run("session.execution.started carries a durable object", func(t *testing.T) {
		col := collectSandboxEvents(ctx, t, client, base, creds.Token, password)

		// resume:true is what schedules the loop; the text is trivial
		// because what is being measured is the frame, not the answer.
		prompt(ctx, t, client, base, creds.Token, password, session,
			"S-LS1: reply with the single word ack.")

		frame, found := col.waitFor(ctx, 3*time.Minute, func(f sseFrame) bool {
			return steerFrameName(f) == "session.execution.started"
		})

		// Logged whether or not the frame arrived, so the ending events'
		// durability is on record from the same run.
		for _, f := range col.seen() {
			name := steerFrameName(f)
			if !strings.HasPrefix(name, "session.execution") {
				continue
			}
			t.Logf("%s: durable=%t  %s", name, steerIsDurable(f), truncate(f.Data, 300))
		}

		if !found {
			t.Fatalf("no session.execution.started frame arrived within 3m."+
				" Either the name is different at this build or the turn never started"+
				" (%d frames seen in total, names: %v);"+
				" DL-059's Stop control has no signal either way",
				len(col.seen()), steerFrameNames(col.seen()))
		}
		if steerIsDurable(frame) {
			t.Logf("confirmed: session.execution.started carries a durable object, so it"+
				" survives projectFrame and reaches the store. DL-059's Stop control has"+
				" its signal: %s", truncate(frame.Data, 400))
			return
		}
		t.Errorf("session.execution.started carries NO durable object, so projectFrame"+
			" drops it and it never reaches the store: DL-059's Stop control can never be"+
			" shown, with nothing logged. The fallback is to derive in-flight from"+
			" the ending events and the job's own state — or the frame joins"+
			" frames.go's ephemeralNames, which is a widening of DL-017's list and its own"+
			" argument. Frame: %s", truncate(frame.Data, 400))
	})
}

// TestIntegrationSLS2InterruptedToolPart is spike S-LS2: what does an
// interrupt leave on an in-flight tool part, and can gate 3 tell it from a
// human's permission reject (DL-053)?
//
// Phase 1 is a warm-up turn, so that finding no tool parts later means the
// interrupt marked nothing rather than that the agent cannot call a tool.
func TestIntegrationSLS2InterruptedToolPart(t *testing.T) {
	steerRequirePin(t)
	p := newIntegrationProvider(t)
	// The sandbox's lifetime below is a hard 15 minutes, so this budget
	// must fit inside it with the boot. A run that outlives its sandbox
	// finds no tool parts and misreports it as "no tool call at all".
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	_, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-sls2", 15*time.Minute, nil)
	client := &http.Client{Timeout: 0}
	base := creds.URL

	// Phase 1, on its own session: does a turn on this image reach an
	// answer at all?
	warmup := createSession(ctx, t, client, base, creds.Token, password)
	warmCol := collectSandboxEvents(ctx, t, client, base, creds.Token, password)
	prompt(ctx, t, client, base, creds.Token, password, warmup,
		"Reply with the single word: ack.")
	_, ok := warmCol.waitFor(ctx, 3*time.Minute, func(f sseFrame) bool {
		return steerFrameName(f) == "session.execution.succeeded" && steerFrameSession(f) == warmup
	})
	t.Logf("warm-up frames: %v", steerFrameNames(warmCol.seen()))
	if !ok {
		t.Logf("warm-up messages: %s",
			truncate(get(ctx, t, client, base+"/api/session/"+warmup+"/message", creds.Token, password), 2000))
		t.Skip("no turn on this image reached session.execution.succeeded within 3m," +
			" so the agent cannot be driven to a tool call and this spike cannot run." +
			" The sandbox needs a working default model or a provider key in its" +
			" environment; the frames and messages above say which.")
	}
	t.Log("confirmed: a turn on this image completes, so a missing tool part below is a finding")

	session := createSession(ctx, t, client, base, creds.Token, password)
	col := collectSandboxEvents(ctx, t, client, base, creds.Token, password)

	// A tool call long enough that the interrupt lands inside it; an
	// interrupt between two calls marks nothing.
	prompt(ctx, t, client, base, creds.Token, password, session, fmt.Sprintf(
		"Use your shell tool to run exactly this command, then tell me what it printed:"+
			" sleep 60; echo %s", steerMarker))

	// The tool call in flight, matched on any frame mentioning a tool
	// because the frame vocabulary at this build is not known.
	if _, running := col.waitFor(ctx, 3*time.Minute, func(f sseFrame) bool {
		return steerFrameSession(f) == session &&
			(strings.Contains(f.Data, steerMarker) || strings.Contains(f.Data, `"tool"`))
	}); !running {
		t.Logf("no frame mentioned a tool within 3m (names seen: %v); interrupting anyway",
			steerFrameNames(col.seen()))
	}

	// A beat, so the interrupt lands in the middle of the command rather
	// than on the boundary the frame announced.
	time.Sleep(5 * time.Second)

	code, raw := steerCall(ctx, t, client, http.MethodPost,
		base+"/api/session/"+session+"/interrupt?resume=true", creds.Token, password, "")
	t.Logf("interrupt during a tool call -> %d: %s", code, truncate(raw, 300))
	if code < 200 || code >= 300 {
		t.Fatalf("the interrupt itself failed (%d): DL-053 has no mechanism", code)
	}
	var out struct {
		Interrupted bool `json:"interrupted"`
	}
	_ = json.Unmarshal([]byte(raw), &out)
	if !out.Interrupted {
		t.Fatalf("interrupted:false — the session was idle when the interrupt landed, so"+
			" no in-flight tool part was marked and this run measures nothing (frames: %v)",
			steerFrameNames(col.seen()))
	}

	// The part is written as the turn unwinds, so it is read after the
	// turn ends, as DL-017's recorder does.
	if _, ended := col.waitFor(ctx, 2*time.Minute, func(f sseFrame) bool {
		name := steerFrameName(f)
		return steerFrameSession(f) == session &&
			(name == "session.execution.interrupted" ||
				name == "session.execution.failed" ||
				name == "session.execution.succeeded")
	}); !ended {
		t.Log("no ending frame within 2m of the interrupt; reading the messages anyway")
	}
	time.Sleep(5 * time.Second)

	// DL-053 relies on session.execution.interrupted being durable and
	// carrying reason "user", so both are asserted rather than logged.
	for _, f := range col.seen() {
		if strings.HasPrefix(steerFrameName(f), "session.execution") && steerFrameSession(f) == session {
			t.Logf("%s: durable=%t  %s", steerFrameName(f), steerIsDurable(f), truncate(f.Data, 400))
		}
	}
	interruptedFrame, sawInterrupted := col.waitFor(ctx, time.Second, func(f sseFrame) bool {
		return steerFrameName(f) == "session.execution.interrupted" && steerFrameSession(f) == session
	})
	if !sawInterrupted {
		t.Errorf("no session.execution.interrupted frame for this session, though the"+
			" interrupt answered interrupted:true. DL-053's wider stranding fix and the"+
			" correlation mechanism both read that frame (names seen: %v)",
			steerFrameNames(col.seen()))
	} else {
		if !steerIsDurable(interruptedFrame) {
			t.Errorf("session.execution.interrupted carries NO durable object, so"+
				" projectFrame drops it and it never reaches the store: the correlation"+
				" mechanism has no input. Frame: %s", truncate(interruptedFrame.Data, 400))
		}
		if reason := steerFrameReason(interruptedFrame); reason != "user" {
			t.Errorf("session.execution.interrupted carries reason %q, not \"user\":"+
				" DL-053 names that value as what separates a human's interrupt from"+
				" OpenCode's own (shutdown, superseded, inactivity)", reason)
		}
	}

	body := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
	parts := toolParts(t, body)
	t.Logf("message store holds part types %v across %d non-text parts",
		steerPartTypes(t, body), len(parts))
	for _, part := range parts {
		t.Logf("tool part: %s", truncate(part, 900))
	}

	// Selected by the command it carries, so an unrelated failed tool call
	// cannot supply the result.
	refused := steerUnrunPart(t, body, steerMarker)
	if refused == nil {
		if other := refusedPart(t, body); other != nil {
			t.Errorf("a part is refused but it is not the one carrying %s"+
				" (type %q, message %q): the discriminator below would have been read"+
				" off an unrelated failure", steerMarker, other.State.Error.Type, other.State.Error.Message)
		}
		if len(parts) == 0 {
			t.Fatalf("the agent made no tool call at all, so the interrupt had nothing"+
				" in flight to mark and this run measures NOTHING. The warm-up proved the"+
				" model answers, so this is the prompt failing to provoke a tool call"+
				" rather than a broken image. Frames seen: %v",
				steerFrameNames(col.seen()))
		}
		t.Fatalf("an interrupt during a tool call left NO part with executed:false and"+
			" status error, across %d tool parts. That is a finding in spov's favour —"+
			" gate 3 would see nothing to discount and no discriminator may be needed at"+
			" all — but confirm it against a second run before building on it, because"+
			" the alternative reading is that the interrupt landed between calls",
			len(parts))
	}

	// S6's measured shape for a human's reject (DL-050).
	const s6Type, s6Message = "aborted", "The user declined this tool call"
	t.Logf("the interrupted part: type=%q message=%q completed=%d",
		refused.State.Error.Type, refused.State.Error.Message, refused.Time.Completed)

	// The part's completion time and the interrupt event's `created`, both
	// on the sandbox's clock, must be close enough that one identifies the
	// other.
	if sawInterrupted && refused.Time.Completed > 0 {
		eventAt := steerFrameCreated(interruptedFrame)
		skew := refused.Time.Completed - eventAt
		if skew < 0 {
			skew = -skew
		}
		if eventAt == 0 {
			t.Errorf("session.execution.interrupted carries no `created`, so there is"+
				" nothing to correlate the part against: %s", truncate(interruptedFrame.Data, 300))
		} else if skew > int64((5 * time.Second).Milliseconds()) {
			t.Errorf("the part completed %d ms from the interrupt event (part %d, event %d),"+
				" which is too wide to identify one from the other: the correlation"+
				" mechanism needs a window, and this says the window is not small",
				skew, refused.Time.Completed, eventAt)
		} else {
			t.Logf("the part's completion and the interrupt event are %d ms apart on the"+
				" SAME clock (part %d, event %d), so an interrupt can be correlated to the"+
				" parts it marked without matching any string",
				skew, refused.Time.Completed, eventAt)
		}
	}

	switch {
	case refused.State.Error.Type != s6Type:
		t.Logf("ANSWERED, and better than expected: an interrupt leaves error.type %q"+
			" while S6's reject leaves %q. the discriminator is the TYPE, which"+
			" toolPart already reads and which DL-050 already treats as a set —"+
			" no change of posture and no new field.",
			refused.State.Error.Type, s6Type)
	case refused.State.Error.Message != s6Message:
		t.Logf("ANSWERED: the type is the same (%q) but the message differs —"+
			" interrupt says %q where S6's reject says %q. the discriminator is"+
			" error.message, which is a CHANGE OF POSTURE: DL-050 documents that field"+
			" as model-adjacent text that is never matched on. Record that in DL-053"+
			" rather than letting it in quietly.",
			refused.State.Error.Type, refused.State.Error.Message, s6Message)
	default:
		t.Errorf("NO DISCRIMINATOR: an interrupt leaves exactly S6's reject shape"+
			" (type %q, message %q). There is no"+
			" mechanism to tell them apart. Check the raw part logged"+
			" below for anything correlating it to a time or a turn before concluding"+
			" there is none; that is the first and cheapest answer.",
			refused.State.Error.Type, refused.State.Error.Message)
	}

	// Logs the raw part's keys, to spot fields toolPart does not read.
	t.Run("what the raw part carries that toolPart drops", func(t *testing.T) {
		for _, raw := range parts {
			var whole map[string]any
			if err := json.Unmarshal([]byte(raw), &whole); err != nil {
				continue
			}
			if fmt.Sprint(whole["type"]) != "tool" {
				continue
			}
			t.Logf("part keys: %v", steerSortedKeys(whole))
			if state, ok := whole["state"].(map[string]any); ok {
				t.Logf("state keys: %v", steerSortedKeys(state))
				if e, ok := state["error"].(map[string]any); ok {
					t.Logf("error keys: %v", steerSortedKeys(e))
				}
			}
		}
	})
}

// steerPartTypes counts what the message store holds by part type, which
// separates an agent that answered in prose from a turn that produced
// nothing.
func steerPartTypes(t *testing.T, body string) map[string]int {
	t.Helper()
	var envelope struct {
		Data []struct {
			Content []json.RawMessage `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return nil
	}
	counts := map[string]int{}
	for _, m := range envelope.Data {
		for _, part := range m.Content {
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(part, &head); err != nil {
				continue
			}
			counts[head.Type]++
		}
	}
	return counts
}

// steerCall does one authenticated request of any method and returns
// the status and body. Unlike `post`, it takes any method and does not
// log.
func steerCall(ctx context.Context, t *testing.T, c *http.Client, method, url, token, password, body string) (int, string) {
	t.Helper()
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := c.Do(sandboxHTTPBody(reqCtx, t, method, url, token, password, body))
	if err != nil {
		// Fatal rather than returning (0, ""), which a dead sandbox
		// would turn into false findings.
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw)
}

// steerInboxIDs lists the ids of a session's undelivered inbox items,
// decoded so the count moves only when the inbox does.
func steerInboxIDs(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string) []string {
	t.Helper()
	code, raw := steerCall(ctx, t, c, http.MethodGet, base+"/api/session/"+session+"/inbox", token, password, "")
	if code < 200 || code >= 300 {
		t.Fatalf("GET /inbox -> %d (%s)", code, truncate(raw, 300))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("the inbox body is not the shape this expects (%v): %s", err, truncate(raw, 300))
	}
	ids := make([]string, 0, len(out.Data))
	for _, item := range out.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

// steerMessageIDFor finds the id the message store gives the message
// carrying this text, reading it the way parseMessages does: the
// top-level `id`, out of a `data` envelope.
//
// It reports false rather than failing when no message carries the
// text, because an item that was enqueued and never delivered has no
// message row yet.
//
// @decision(DL-059) @decision(DL-019)
func steerMessageIDFor(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session, text string) (string, bool) {
	t.Helper()
	code, raw := steerCall(ctx, t, c, http.MethodGet, base+"/api/session/"+session+"/message", token, password, "")
	if code < 200 || code >= 300 {
		t.Fatalf("GET /message -> %d (%s)", code, truncate(raw, 300))
	}
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("the message body is not the shape this expects (%v): %s", err, truncate(raw, 300))
	}
	for _, m := range out.Data {
		if strings.Contains(m.Text, text) {
			return m.ID, m.ID != ""
		}
	}
	return "", false
}

// steerPromptRecord is what POST /prompt answers with — the
// SessionInbox.User record, under a `data` envelope like every other
// session body.
type steerPromptRecord struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Time      struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Type     string `json:"type"`
	Delivery string `json:"delivery"`
}

func steerPromptRecordOf(t *testing.T, body string) steerPromptRecord {
	t.Helper()
	var envelope struct {
		Data steerPromptRecord `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("the prompt answer is not the shape DL-019 records (%v): %s", err, truncate(body, 400))
	}
	return envelope.Data
}

// steerToolPart is a raw tool part as the message store holds it, decoded
// independently of internal/jobs' toolPart.
type steerToolPart struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Executed bool   `json:"executed"`
	State    struct {
		Status string `json:"status"`
		Input  struct {
			Command string `json:"command"`
		} `json:"input"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
	Time struct {
		Created   int64 `json:"created"`
		Ran       int64 `json:"ran"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// steerUnrunPart finds the tool part for the command under test that did
// not run and is marked in error, whatever marked it. An interrupt and a
// human reject both leave that shape.
func steerUnrunPart(t *testing.T, body, marker string) *steerToolPart {
	t.Helper()
	for _, raw := range toolParts(t, body) {
		var p steerToolPart
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}
		if p.Type != "tool" || p.Executed || p.State.Status != "error" {
			continue
		}
		if !strings.Contains(p.State.Input.Command, marker) {
			continue
		}
		return &p
	}
	return nil
}

// steerFrameReason and steerFrameCreated read the two fields DL-053's
// interrupt correlation rests on.
func steerFrameReason(f sseFrame) string {
	var frame struct {
		Data struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return ""
	}
	return frame.Data.Reason
}

func steerFrameCreated(f sseFrame) int64 {
	var frame struct {
		Created int64 `json:"created"`
	}
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return 0
	}
	return frame.Created
}

// steerDoc is the part of the sandbox's own OpenAPI document these
// questions read.
type steerDoc struct {
	Paths      map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

func steerOpenAPI(ctx context.Context, t *testing.T, c *http.Client, base, token, password string) steerDoc {
	t.Helper()
	raw := get(ctx, t, c, base+"/openapi.json", token, password)
	var doc steerDoc
	if raw == "" {
		return doc
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Logf("the route document is not JSON: %v", err)
	}
	return doc
}

// steerRoutes keeps the session paths the steering batch cares about.
func steerRoutes(doc steerDoc) []string {
	var out []string
	for path := range doc.Paths {
		if !strings.Contains(path, "/session/") {
			continue
		}
		if strings.Contains(path, "prompt") || strings.Contains(path, "interrupt") ||
			strings.Contains(path, "inbox") {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// steerHasRoute matches a declared path against a shape, comparing the
// literal segments and treating any {placeholder} as a wildcard, since
// the document names its parameters its own way.
func steerHasRoute(declared []string, shape string) bool {
	want := strings.Split(strings.Trim(shape, "/"), "/")
	for _, path := range declared {
		got := strings.Split(strings.Trim(path, "/"), "/")
		// The document prefixes its own base, so the shape is matched
		// against the tail rather than the whole.
		if len(got) < len(want) {
			continue
		}
		got = got[len(got)-len(want):]
		match := true
		for i := range want {
			if strings.HasPrefix(want[i], "{") && strings.HasPrefix(got[i], "{") {
				continue
			}
			if want[i] != got[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// steerDeliveryEnum finds whichever schema carries a `delivery`
// property with an enum, rather than guessing the schema's name, which
// can change between builds.
func steerDeliveryEnum(t *testing.T, doc steerDoc) (string, []string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(doc.Components.Schemas[name], &schema); err != nil {
			continue
		}
		prop, ok := schema.Properties["delivery"]
		if !ok {
			continue
		}
		if values := enumValues(prop, doc.Components.Schemas); len(values) > 0 {
			return name, values
		}
	}
	return "", nil
}

// steerFrameName reads a frame's event name from its top-level `event` or
// `type`, falling back to the SSE event field.
func steerFrameName(f sseFrame) string {
	var frame map[string]any
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return f.Event
	}
	for _, key := range []string{"event", "type"} {
		if v, ok := frame[key].(string); ok && v != "" {
			return v
		}
	}
	return f.Event
}

// steerFrameSession reads the session a frame belongs to, which matters
// because /api/event is the sandbox's global stream.
func steerFrameSession(f sseFrame) string {
	var frame struct {
		Data struct {
			SessionID string `json:"sessionID"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return ""
	}
	return frame.Data.SessionID
}

// steerIsDurable answers the question projectFrame asks: does the frame
// carry a `durable` object? Presence is the marker, not a value.
func steerIsDurable(f sseFrame) bool {
	var frame map[string]any
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return false
	}
	switch v := frame["durable"].(type) {
	case map[string]any:
		return len(v) > 0
	case string:
		return v != ""
	}
	return false
}

func steerFrameNames(frames []sseFrame) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range frames {
		name := steerFrameName(f)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func steerSortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// sandboxEvents holds one /api/event stream open and accumulates what
// it sees, so a test can act (here, interrupt) while the stream is open,
// which readFrames cannot serve because it blocks until its frame budget
// is spent.
type sandboxEvents struct {
	mu     sync.Mutex
	frames []sseFrame
	notes  []string
	done   chan struct{}
}

// note records something about the STREAM rather than about a frame.
// It is read by the test goroutine, so it cannot be a t.Logf from the
// reader.
func (s *sandboxEvents) note(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, msg)
}

// report says what the stream did, separating a quiet sandbox from a dead
// stream. A report taken while the stream is still open says so: t.Cleanup
// runs LIFO, so a caller's cleanup fires before the collector's.
func (s *sandboxEvents) report() []string {
	s.mu.Lock()
	notes := slices.Clone(s.notes)
	n := len(s.frames)
	s.mu.Unlock()
	select {
	case <-s.done:
	default:
		notes = append(notes, fmt.Sprintf("stream still open, %d frames so far", n))
	}
	return notes
}

func collectSandboxEvents(ctx context.Context, t *testing.T, c *http.Client, base, token, password string) *sandboxEvents {
	t.Helper()
	ectx, cancel := context.WithCancel(ctx)
	resp, err := c.Do(sandboxHTTP(ectx, t, http.MethodGet, base+"/api/event", token, password))
	if err != nil {
		cancel()
		t.Fatalf("/api/event: %v", err)
	}
	col := &sandboxEvents{done: make(chan struct{})}
	go func() {
		defer close(col.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		defer func() {
			// Record the end, so a dead stream is not read as a quiet
			// sandbox.
			col.mu.Lock()
			n := len(col.frames)
			col.mu.Unlock()
			col.note(fmt.Sprintf("stream ended after %d frames (scanner err: %v)", n, sc.Err()))
		}()
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Event != "" || cur.Data != "" {
					col.mu.Lock()
					col.frames = append(col.frames, cur)
					col.mu.Unlock()
					cur = sseFrame{}
				}
			case strings.HasPrefix(line, "event:"):
				cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				cur.Data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-col.done
	})
	return col
}

func (s *sandboxEvents) seen() []sseFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.frames)
}

// waitFor polls what has arrived until a frame matches or `within`
// passes. It returns the first match.
func (s *sandboxEvents) waitFor(ctx context.Context, within time.Duration, match func(sseFrame) bool) (sseFrame, bool) {
	deadline := time.Now().Add(within)
	for {
		for _, f := range s.seen() {
			if match(f) {
				return f, true
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return sseFrame{}, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForAfter is waitFor scoped to frames at index `from` onwards, for
// questions about one turn rather than the whole stream.
func (s *sandboxEvents) waitForAfter(ctx context.Context, within time.Duration, from int, match func(sseFrame) bool) (sseFrame, bool) {
	deadline := time.Now().Add(within)
	for {
		frames := s.seen()
		if from < len(frames) {
			for _, f := range frames[from:] {
				if match(f) {
					return f, true
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return sseFrame{}, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// The two commands S-LS3 drives: one is parked by the plugin below and
// rejected by a human, the other runs long enough to be interrupted.
// Matched by marker so an unrelated tool failure cannot supply either half.
const (
	sls3RejectMarker    = "spov-sls3-reject-me"
	sls3InterruptMarker = "spov-sls3-interrupt-me"
)

// sls3PluginJS parks only on the reject marker. Parking both would leave
// the interrupt's command waiting on a permission, with nothing in flight
// to interrupt.
var sls3PluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})
    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        const asked = peek(evaluation?.resources, 400) + " " + String(evaluation?.action)
        if (asked.includes("` + sls3RejectMarker + `")) {
          try { evaluation.effect = "ask" } catch (e) {}
          say("permission.parked", { asked: peek(evaluation?.resources, 200) })
        }
      })
      say("permission.hooked", {})
    } catch (e) {
      say("permission.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationSLS3InterruptVersusReject is spike S-LS3: in one session on
// one build, a shell call a human rejects through the permission reply and one
// spov interrupts mid-flight, compared on error.type, error.message and their
// correlation with session.execution.interrupted. One session because a job is
// one conversation, and gate 3 has to tell one part from another inside it.
// Results are in DL-053.
//
//	go test -tags integration ./internal/sandbox/ -run SLS3 -v
func TestIntegrationSLS3InterruptVersusReject(t *testing.T) {
	steerRequirePin(t)
	p := newIntegrationProvider(t)
	// The sandbox's lifetime is a hard 15 minutes, so the whole run has
	// to fit inside it with the boot.
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	_, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-sls3", 15*time.Minute,
		map[string]string{spikePluginPath: sls3PluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL

	session := createSession(ctx, t, client, base, creds.Token, password)
	col := collectSandboxEvents(ctx, t, client, base, creds.Token, password)

	// ---- half one: a human refuses a tool call ----------------------
	//
	// No warm-up turn, unlike S-LS2: a permission can only park after a
	// tool call, so reaching the reject proves the model works.
	prompt(ctx, t, client, base, creds.Token, password, session, fmt.Sprintf(
		"Use your shell tool to run exactly this command, then tell me what it printed:"+
			" echo %s", sls3RejectMarker))

	asked, parked := col.waitFor(ctx, 4*time.Minute, func(f sseFrame) bool {
		return steerFrameName(f) == "permission.asked" && steerFrameSession(f) == session
	})
	if !parked {
		t.Fatalf("nothing parked within 4m, so there is no permission to refuse and this"+
			" run measures none of the comparison. Either the agent made no tool call —"+
			" which means the image has no working model — or the hook did not fire."+
			" Frames seen: %v", steerFrameNames(col.seen()))
	}
	permission := steerPermissionID(asked)
	if permission == "" {
		t.Fatalf("the permission frame names no id, so there is nothing to address an"+
			" answer to: %s", truncate(asked.Data, 500))
	}
	t.Logf("parked on permission %s", permission)

	// The document's own refusal value first, then the two usual
	// spellings.
	refusal := ""
	for _, v := range schemaEnum(ctx, t, client, base, creds.Token, password, "Permission.Reply") {
		if v == "reject" || v == "deny" || v == "never" {
			refusal = v
			break
		}
	}
	rejected := false
	for _, v := range []string{refusal, "reject", "deny"} {
		if v == "" {
			continue
		}
		code, raw := steerCall(ctx, t, client, http.MethodPost,
			base+"/api/session/"+session+"/permission/"+permission+"/reply",
			creds.Token, password, fmt.Sprintf(`{"decision":%q}`, v))
		t.Logf("rejecting with %q -> %d %s", v, code, truncate(raw, 200))
		if code >= 200 && code < 300 {
			rejected = true
			break
		}
	}
	if !rejected {
		t.Fatal("no refusal value was accepted, so the human half of the comparison never" +
			" happened. S6 measured this route working at 18684; a 404 here is a route" +
			" change and a 400 is a body change, and the log above says which")
	}

	// Read after the turn ends, as in S-LS2.
	if _, ended := col.waitFor(ctx, 3*time.Minute, func(f sseFrame) bool {
		return steerFrameSession(f) == session && steerTurnEnded(steerFrameName(f))
	}); !ended {
		t.Log("no ending frame within 3m of the rejection; continuing anyway")
	}
	time.Sleep(5 * time.Second)
	// Waits below are scoped to what arrives after this point, since the
	// refused turn made a tool call too.
	afterReject := len(col.seen())

	// ---- half two: spov interrupts a tool call ----------------------
	prompt(ctx, t, client, base, creds.Token, password, session, fmt.Sprintf(
		"Now use your shell tool to run exactly this command, then tell me what it printed:"+
			" sleep 60; echo %s", sls3InterruptMarker))

	// Wait for the turn to start: the prompt returns once the item is
	// enqueued, not once the model has read it (DL-019).
	if _, started := col.waitForAfter(ctx, 3*time.Minute, afterReject, func(f sseFrame) bool {
		return steerFrameSession(f) == session &&
			steerFrameName(f) == "session.execution.started"
	}); !started {
		t.Logf("the second turn did not start within 3m (frames since the reject: %v)",
			steerFrameNames(col.seen()[afterReject:]))
	}

	// Then wait for the tool call in the message store, not the frame
	// stream: a frame carrying the marker may be the prompt itself, and
	// one mentioning a tool may be the previous turn. `sleep 60` gives
	// the interrupt a minute of runway once the part appears.
	var inflight *steerToolPart
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); {
		probe := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
		if p := steerPartWithCommand(t, probe, sls3InterruptMarker); p != nil {
			inflight = p
			break
		}
		time.Sleep(2 * time.Second)
	}
	if inflight == nil {
		t.Logf("no tool part carrying %s appeared within 3m, so the agent has not made the"+
			" call and the interrupt below has nothing in flight to mark. Frames since the"+
			" reject: %v", sls3InterruptMarker, steerFrameNames(col.seen()[afterReject:]))
	} else {
		t.Logf("the interrupt's own call is in the store with status %q: %s",
			inflight.State.Status, truncate(inflight.State.Input.Command, 120))
	}

	code, raw := steerCall(ctx, t, client, http.MethodPost,
		base+"/api/session/"+session+"/interrupt?resume=true", creds.Token, password, "")
	t.Logf("interrupt during a tool call -> %d: %s", code, truncate(raw, 300))
	if code < 200 || code >= 300 {
		t.Fatalf("the interrupt itself failed (%d): DL-053 has no mechanism", code)
	}
	var out struct {
		Interrupted bool `json:"interrupted"`
	}
	_ = json.Unmarshal([]byte(raw), &out)
	if !out.Interrupted {
		t.Fatalf("interrupted:false — the session was idle when the interrupt landed, so" +
			" nothing in flight was marked and the second half measures nothing")
	}
	if _, ended := col.waitFor(ctx, 2*time.Minute, func(f sseFrame) bool {
		name := steerFrameName(f)
		return steerFrameSession(f) == session &&
			(name == "session.execution.interrupted" || steerTurnEnded(name))
	}); !ended {
		t.Log("no ending frame within 2m of the interrupt; reading the messages anyway")
	}
	time.Sleep(5 * time.Second)

	// ---- the comparison ---------------------------------------------
	body := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
	t.Logf("the session's store holds part types %v", steerPartTypes(t, body))

	refused := steerUnrunPart(t, body, sls3RejectMarker)
	stopped := steerUnrunPart(t, body, sls3InterruptMarker)

	// Both halves are logged before anything can end the run.
	if refused != nil {
		t.Logf("HUMAN REJECT:   type=%q message=%q completed=%d",
			refused.State.Error.Type, refused.State.Error.Message, refused.Time.Completed)
	}
	if stopped != nil {
		t.Logf("SPOV INTERRUPT: type=%q message=%q completed=%d",
			stopped.State.Error.Type, stopped.State.Error.Message, stopped.Time.Completed)
	}
	for _, raw := range toolParts(t, body) {
		t.Logf("tool part: %s", truncate(raw, 900))
	}

	switch {
	case refused == nil && stopped == nil:
		t.Fatalf("neither half left a part that did not run, across %d tool parts:"+
			" this run compared nothing", len(toolParts(t, body)))
	case refused == nil:
		t.Fatal("the REJECTED call left no unrun part, so there is nothing to compare the" +
			" interrupt against. S6 measured this shape at 18684; its absence here is" +
			" itself a finding — gate 3 reads that shape and would see no refusal at all")
	case stopped == nil:
		t.Fatal("the INTERRUPTED call left no unrun part. Before reading that as a" +
			" finding in spov's favour, check whether the call was in flight at all:" +
			" an interrupt that landed BETWEEN calls marks nothing and looks identical" +
			" to one that marks nothing by design. S-LS2 measured a marked part at this" +
			" pin, so the likelier reading is the timing")
	}

	// The event must identify the part spov stopped and not the one a
	// human refused. A reject raises session.execution.interrupted too,
	// so the frames are grouped by reason rather than counted.
	interrupts := col.seenNamed("session.execution.interrupted", session)
	t.Logf("session.execution.interrupted frames for this session: %d", len(interrupts))
	byReason := map[string][]sseFrame{}
	for _, f := range interrupts {
		reason := steerFrameReason(f)
		byReason[reason] = append(byReason[reason], f)
		t.Logf("  reason=%q created=%d durable=%t  %s",
			reason, steerFrameCreated(f), steerIsDurable(f), truncate(f.Data, 300))
	}

	window := int64((5 * time.Second).Milliseconds())
	gap := func(at int64, p *steerToolPart) int64 {
		d := p.Time.Completed - at
		if d < 0 {
			d = -d
		}
		return d
	}
	// The negative half, and the one that decides the mechanism: does
	// correlating on the EVENT alone sweep up the human's refusal?
	for reason, frames := range byReason {
		if reason == "user" {
			continue
		}
		for _, f := range frames {
			if at := steerFrameCreated(f); at > 0 && gap(at, refused) <= window {
				t.Logf("NOTE: a human's reject raises session.execution.interrupted too,"+
					" reason %q, %d ms from the part it refused. Correlating on the EVENT"+
					" alone cannot discriminate — the reason is load-bearing, not"+
					" decoration.", reason, gap(at, refused))
			}
		}
	}

	correlates := false
	users := byReason["user"]
	switch len(users) {
	case 0:
		t.Error(`no session.execution.interrupted carried reason "user", though S-LS2` +
			" measured one at this pin. mechanism (2) has no input it can trust," +
			" so only the string match is left, or nothing")
	case 1:
		at := steerFrameCreated(users[0])
		stoppedNear, refusedNear := gap(at, stopped), gap(at, refused)
		t.Logf(`the reason="user" event is %d ms from the INTERRUPTED part and %d ms from`+
			" the REJECTED one", stoppedNear, refusedNear)
		switch {
		case at == 0 || stopped.Time.Completed == 0:
			t.Errorf("no usable clock: event created=%d, part completed=%d",
				at, stopped.Time.Completed)
		case stoppedNear > window:
			t.Errorf("the INTERRUPTED part is %d ms from the event, wider than the %d ms"+
				" window: correlation cannot identify the part it marked",
				stoppedNear, window)
		case refusedNear <= window:
			t.Errorf("the REJECTED part is ALSO within %d ms of it (%d ms): the two cannot"+
				" be told apart by time, which is the whole of what mechanism (2) claims",
				window, refusedNear)
		default:
			correlates = true
			t.Logf(`CONFIRMED: the reason="user" event identifies the part spov stopped`+
				" (%d ms) and not the one a human refused (%d ms), on ONE clock and in"+
				" ONE session. Mechanism (2) works — filtered by reason.",
				stoppedNear, refusedNear)
		}
	default:
		t.Errorf(`%d events carried reason="user" for one interrupt. A window around each`+
			" would sweep up parts nobody stopped, so a rule would be needed for which"+
			" of them owns a part", len(users))
	}

	// ---- the verdict ------------------------------------------------
	sameType := refused.State.Error.Type == stopped.State.Error.Type
	sameMessage := refused.State.Error.Message == stopped.State.Error.Message
	switch {
	case !sameType:
		t.Logf("ANSWERED — mechanism (1) by TYPE, the cheapest outcome. A reject leaves"+
			" %q and an interrupt leaves %q, measured in ONE run on ONE build. toolPart"+
			" already reads error.type and DL-050 already treats it as a set, so the check is"+
			" a list entry: no new field, no change of posture.",
			refused.State.Error.Type, stopped.State.Error.Type)
	case !sameMessage:
		t.Logf("ANSWERED — the types agree (%q) and the MESSAGES differ: reject says %q,"+
			" interrupt says %q. This is the pairing that did not exist before, since"+
			" S6 read its wording at 18684 and S-LS2 read the other at 19151. Mechanism"+
			" (1) is available but it is a string match on text DL-050 documents as"+
			" never matched on, and nothing would catch a rewording.",
			refused.State.Error.Type, refused.State.Error.Message, stopped.State.Error.Message)
		if correlates {
			t.Log("PREFER mechanism (2), WITH the reason filter: it needs no string, both" +
				" wordings come from the model-adjacent side of the part, and the run" +
				" above shows the filter is what makes it work at all. Correlating on" +
				" the event alone is measurably wrong. Use the message as corroboration" +
				" if a second signal is wanted — option (3).")
		}
	case correlates:
		t.Logf("ANSWERED — mechanism (2) ALONE. An interrupt and a human reject are"+
			" identical in the part (type %q, message %q), so no string can separate"+
			" them and option (1) is dead. The correlation above is confirmed and"+
			" is what to build on.",
			refused.State.Error.Type, refused.State.Error.Message)
	default:
		t.Errorf("NO DISCRIMINATOR. The parts are identical (type %q, message %q) and the"+
			" correlation did not hold either, so all three options are"+
			" exhausted: there is no mechanism. Before"+
			" concluding that, read the raw parts logged below for any field toolPart"+
			" drops — that is the first and cheapest answer and it is what turned S-LS2"+
			" around.", refused.State.Error.Type, refused.State.Error.Message)
		for _, raw := range toolParts(t, body) {
			t.Logf("raw part: %s", truncate(raw, 1200))
		}
	}
}

// steerPartWithCommand finds a tool part by the command it carries,
// whatever state it is in. It answers whether the call has been made at
// all, which an interrupt has to wait for.
func steerPartWithCommand(t *testing.T, body, marker string) *steerToolPart {
	t.Helper()
	for _, raw := range toolParts(t, body) {
		var p steerToolPart
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}
		if p.Type != "tool" || !strings.Contains(p.State.Input.Command, marker) {
			continue
		}
		return &p
	}
	return nil
}

// steerPermissionID reads the permission's own id off a
// permission.asked frame.
//
// It is `data.id` (per_…), not the frame's top-level `id`, which is the
// event's (evt_…).
func steerPermissionID(f sseFrame) string {
	var frame struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return ""
	}
	return frame.Data.ID
}

// steerTurnEnded is the frame vocabulary that closes a turn, matched by
// suffix as internal/jobs and the recorder do.
func steerTurnEnded(name string) bool {
	return strings.HasSuffix(name, ".execution.succeeded") ||
		strings.HasSuffix(name, ".execution.failed") ||
		strings.HasSuffix(name, ".execution.interrupted")
}

// seenNamed returns every frame of a name belonging to one session.
func (s *sandboxEvents) seenNamed(name, session string) []sseFrame {
	var out []sseFrame
	for _, f := range s.seen() {
		if steerFrameName(f) == name && steerFrameSession(f) == session {
			out = append(out, f)
		}
	}
	return out
}
