package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The goal handler is registered for the compaction summariser as well
// as for primary requests, and pushes the goal into the system[] shape
// both builds were measured to send: {type: "text", text} objects.
//
// @decision(DL-039)
func TestTheGoalReachesCompactionAsWellAsEveryRequest(t *testing.T) {
	runtime := jsRuntime(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")
	p := Plugin{Kind: "report", Goal: "the standing goal"}
	source, err := p.Source()
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence.jsonl")
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	source = strings.Replace(source, "export default {", "const __spovDefault = {", 1)
	source += `
import * as __fs from "node:fs"
const __hooks = {}
const __ctx = {
  session: { hook: (name, fn) => { __hooks[name] = fn }, synthetic: async () => {} },
  permission: { hook: () => {} },
  command: { transform: () => {} },
  tool: { transform: () => {} },
  event: { subscribe: () => ({ [Symbol.asyncIterator]: () => ({ next: async () => ({ done: true }) }) }) },
}
await __spovDefault.setup(__ctx)
const __out = {}
for (const name of ["context", "compaction"]) {
  const event = { system: [{ type: "text", text: "the harness's own" }] }
  if (__hooks[name]) __hooks[name](event)
  __out[name] = event.system
}
__fs.writeFileSync(` + jsonString(t, out) + `, JSON.stringify(__out))
`
	script := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, err := exec.Command(runtime, script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the plugin: %v\n%s", err, stdout)
	}
	hooked := map[string]bool{}
	for _, line := range readJSONLines(t, evidence, string(stdout)) {
		switch line["event"] {
		case "goal.hooked":
			hooked[fmt.Sprint(line["hook"])] = true
		case "goal.failed":
			t.Errorf("a goal hook failed to attach: %v", line)
		}
	}
	if !hooked["context"] || !hooked["compaction"] {
		t.Errorf("the evidence records the goal attached to %v, want context and compaction", hooked)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"context", "compaction"} {
		system := got[name]
		if len(system) != 2 {
			t.Errorf("%s: system = %v, want the goal pushed after the existing entry", name, system)
			continue
		}
		last := system[1]
		if last["type"] != "text" || !strings.Contains(last["text"].(string), "the standing goal") {
			t.Errorf("%s: pushed %v, want a text entry carrying the goal", name, last)
		}
	}
}
