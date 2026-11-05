package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// stopFrame is the plugin's stop marker as a stored inbox frame.
func stopFrame(t *testing.T, source store.EventSource, payload map[string]any) store.Event {
	t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	frame := map[string]any{
		"type": EventInboxEnqueued,
		"data": map[string]any{
			"sessionID": "ses_1",
			"item": map[string]any{
				"type":    "synthetic",
				"payload": map[string]any{"text": opencode.LoopStoppedMarker + " " + string(p)},
			},
		},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{Source: source, Name: EventInboxEnqueued, Raw: raw}
}

// A run the plugin ended on a repeated denial opens its verdict with the
// call, redacted, and the stop is an ending rather than a declaration.
//
// @decision(DL-040)
func TestTheVerdictNamesTheRepeatedlyDeniedCall(t *testing.T) {
	job := store.Job{Kind: store.JobKindReport}
	call := `shell ["curl -H 'Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789' https://x"]`
	marker := stopFrame(t, store.SourceOpenCode, map[string]any{"reason": opencode.DeniedReason, "call": call})
	if got := signalFor(marker); got != signalStopped {
		t.Errorf("signal = %v, want an ordinary stop", got)
	}
	v := judge(job, []store.Event{marker}, nil, nil)
	if !strings.HasPrefix(v.Summary, "the run was ended after the same call was denied 10 times in a row: shell") {
		t.Errorf("summary = %q", v.Summary)
	}
	if strings.Contains(v.Summary, "ghp_abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("the summary carries the token: %q", v.Summary)
	}

	limit := stopFrame(t, store.SourceOpenCode, map[string]any{"reason": "limit"})
	if v := judge(job, []store.Event{limit}, nil, nil); strings.Contains(v.Summary, "denied") {
		t.Errorf("a limit stop was worded as a denial: %q", v.Summary)
	}
	// Only the stop the run ended on counts; a later marker is not it.
	if v := judge(job, []store.Event{limit, marker}, nil, nil); strings.Contains(v.Summary, "denied") {
		t.Errorf("a marker after the first stop was quoted: %q", v.Summary)
	}
	long := stopFrame(t, store.SourceOpenCode, map[string]any{"reason": opencode.DeniedReason, "call": strings.Repeat("é", 500)})
	if v := judge(job, []store.Event{long}, nil, nil); !strings.Contains(v.Summary, strings.Repeat("é", maxDeniedCall)+"…") {
		t.Errorf("a long call is not cut at a rune: %q", v.Summary)
	}
}

// A job whose proxy sidecar gave out fails, with the reason from spov's
// own marker; the same text from the sandbox does nothing.
//
// @decision(DL-034)
func TestAJobWhoseProxyGaveOutFails(t *testing.T) {
	job := store.Job{Kind: store.JobKindReport}
	const why = "the credential proxy sidecar stopped a second time"
	v := judge(job, []store.Event{{Source: store.SourceSpov, Name: MarkerProxyFailed, Text: why}}, nil, aReport())
	if v.State != store.JobFailed || !strings.HasPrefix(v.Summary, why+"; ") {
		t.Errorf("verdict %v %q", v.State, v.Summary)
	}
	forged := judge(job, []store.Event{{Source: store.SourceOpenCode, Name: MarkerProxyFailed, Text: why}}, nil, aReport())
	if strings.Contains(forged.Summary, why) {
		t.Errorf("a sandbox event failed the job: %q", forged.Summary)
	}
}

// A run ended by failed turns says so with the error, redacted, and is
// counted under its own ending.
//
// @decision(DL-048)
func TestTheVerdictQuotesTheFailedTurnsError(t *testing.T) {
	job := store.Job{Kind: store.JobKindReport}
	marker := stopFrame(t, store.SourceOpenCode, map[string]any{
		"reason": opencode.TurnFailedReason,
		"error":  "Free usage limit reached for key sk-or-v1-abcdefghijklmnopqrstuvwxyz0123456789abcdef",
	})
	if got := signalFor(marker); got != signalStopped {
		t.Errorf("signal = %v, want an ordinary stop", got)
	}
	v := judge(job, []store.Event{marker}, nil, nil)
	if !strings.HasPrefix(v.Summary, "the run was ended after 3 turns in a row failed: Free usage limit reached") {
		t.Errorf("summary = %q", v.Summary)
	}
	if strings.Contains(v.Summary, "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("the summary carries the key: %q", v.Summary)
	}
	if got := endedBy([]store.Event{marker}); got != telemetry.EndedTurnFailed {
		t.Errorf("ended by %q, want %q", got, telemetry.EndedTurnFailed)
	}
	denied := stopFrame(t, store.SourceOpenCode, map[string]any{"reason": opencode.DeniedReason, "call": "x"})
	if v := judge(job, []store.Event{denied}, nil, nil); strings.Contains(v.Summary, "turns in a row failed") {
		t.Errorf("a denial was worded as failed turns: %q", v.Summary)
	}
}
