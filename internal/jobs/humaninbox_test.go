package jobs

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// humanInboxFrames are the frames both rules are asked about. The first
// is a captured one (messageid_test.go's realInboxFrame); the rest vary
// what findMap and the plugin's findItem have to agree on.
var humanInboxFrames = []string{
	`{"id":"evt_0000000000sampleevent00001","created":1788267414303,` +
		`"type":"session.inbox.enqueued","location":{"directory":"/workspace/spov"},` +
		`"data":{"inboxID":"msg_0000000000sampleinbox00001","sessionID":"ses_0000000000samplesession000",` +
		`"item":{"type":"user","payload":{"text":"reply with ok"},"delivery":"steer"}},` +
		`"durable":{"aggregateID":"ses_0000000000samplesession000","seq":30,"version":1}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"type":"synthetic","payload":{"text":"continue"}}}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"type":"user","delivery":"queue"}}}`,
	`{"type":"session.inbox.enqueued","properties":{"item":{"type":"user"}}}`,
	`{"type":"session.inbox.enqueued","data":{"wrap":{"item":{"type":"user"}}}}`,
	`{"type":"session.inbox.enqueued","data":{"a":{"b":{"c":{"d":{"item":{"type":"user"}}}}}}}`,
	`{"type":"session.inbox.enqueued","data":{"a":{"item":{"type":"synthetic"}},"item":{"type":"user"}}}`,
	`{"type":"session.inbox.enqueued","data":{"a":{"item":{"type":"user"}},"b":{"item":{"type":"synthetic"}}}}`,
	`{"type":"session.inbox.enqueued","data":{"item":[{"type":"user"}]}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"payload":{"text":"no type"}}}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"type":"User"}}}`,
	`{"type":"session.inbox.delivered","data":{"item":{"type":"user"}}}`,
	`{"type":"session.inbox.enqueued"}`,
	`{"type":"session.inbox.enqueued","data":{"inboxID":"msg_a","item":{"type":"user","payload":{"text":"go on","metadata":{"spov":{"after":"continue"}}},"delivery":"queue"}}}`,
	`{"type":"session.inbox.enqueued","data":{"inboxID":"msg_b","item":{"type":"user","payload":{"text":"over to me","metadata":{"spov":{"after":"handover"}}},"delivery":"queue"}}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"type":"user","payload":{"metadata":{"spov":{"after":"Continue"}}}}}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"type":"user","payload":{"metadata":{"other":{"after":"continue"}}}}}}`,
	`{"type":"session.inbox.enqueued","data":{"item":{"type":"synthetic","payload":{"metadata":{"spov":{"after":"handover"}}}}}}`,
}

// The plugin holds its loop on exactly the items the supervisor reads as
// a person taking the job over, and both read the same inbox id. If the
// two drift, a message either steers a job whose loop keeps continuing,
// or holds a loop the supervisor never steers.
//
// @decision(DL-052) @decision(DL-046)
func TestThePluginAgreesOnWhatTakesOver(t *testing.T) {
	runtime := ""
	for _, name := range []string{"bun", "node"} {
		if p, err := exec.LookPath(name); err == nil {
			runtime = p
			break
		}
	}
	if runtime == "" {
		t.Skip("neither bun nor node is installed; this runs the plugin's own JavaScript")
	}

	src, err := opencode.Plugin{Kind: "report", Goal: "g"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := json.Marshal(humanInboxFrames)
	if err != nil {
		t.Fatal(err)
	}
	src += "\nconst __frames = " + string(fixtures) + "\n" +
		"console.log('SPOV_TEST ' + JSON.stringify(__frames.map((f) => [takesOver(JSON.parse(f)), inboxIDOf(JSON.parse(f))])))\n" +
		"process.exit(0)\n"
	script := filepath.Join(t.TempDir(), "plugin.mjs")
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(runtime, script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the plugin: %v\n%s", err, out)
	}
	type answer struct {
		Over bool
		ID   string
	}
	var plugin []answer
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "SPOV_TEST "); ok {
			var pairs [][2]any
			if err := json.Unmarshal([]byte(rest), &pairs); err != nil {
				t.Fatalf("the plugin's answer is not JSON: %v\n%s", err, out)
			}
			for _, p := range pairs {
				over, _ := p[0].(bool)
				id, _ := p[1].(string)
				plugin = append(plugin, answer{over, id})
			}
		}
	}
	if len(plugin) != len(humanInboxFrames) {
		t.Fatalf("the plugin answered %d frames of %d:\n%s", len(plugin), len(humanInboxFrames), out)
	}

	humans := 0
	for i, raw := range humanInboxFrames {
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		goSays := takesOver(store.Event{Name: frame.Type, Raw: []byte(raw)})
		if goSays {
			humans++
		}
		if plugin[i].Over != goSays {
			t.Errorf("frame %d: takesOver = %v, the plugin says %v\n%s", i, goSays, plugin[i].Over, raw)
		}
		if id := InboxID([]byte(raw)); plugin[i].ID != id {
			t.Errorf("frame %d: InboxID = %q, the plugin says %q\n%s", i, id, plugin[i].ID, raw)
		}
	}
	// Both halves must be exercised, or agreement proves nothing.
	if humans == 0 || humans == len(humanInboxFrames) {
		t.Fatalf("%d of %d frames are human; the fixtures do not test both answers", humans, len(humanInboxFrames))
	}
}
