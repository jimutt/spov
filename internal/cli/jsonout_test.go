package cli

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
)

// decodeOnly decodes out into v and fails unless out is that one JSON
// value and nothing else.
func decodeOnly(t *testing.T, out string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries more than the JSON:\n%s", out)
	}
}

// --json is on exactly the commands DL-023 names.
//
// @decision(DL-023)
func TestTheReadAndCreateCommandsTakeJSON(t *testing.T) {
	want := map[string]bool{}
	for _, path := range []string{
		"ls", "jobs", "jobs list", "jobs show", "jobs create", "credentials", "credentials list", "whoami",
	} {
		want["spov "+path] = true
	}
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		has := cmd.Flags().Lookup("json") != nil
		if has != want[cmd.CommandPath()] {
			t.Errorf("`%s` takes --json: %v, want %v", cmd.CommandPath(), has, want[cmd.CommandPath()])
		}
		delete(want, cmd.CommandPath())
		for _, c := range cmd.Commands() {
			visit(c)
		}
	}
	visit(NewRoot(Env{}))
	for path := range want {
		t.Errorf("`%s` is not in the tree", path)
	}
}

// `ls --json` is the listing's body with its cursor and total, holding
// what the table holds, and an empty one is an empty list rather than
// null.
//
// @decision(DL-023) @decision(DL-012)
func TestLsJSONIsTheSessionListing(t *testing.T) {
	f := newAPIFixture(t)
	out, _, err := runWith(t, f.env, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != `{`+"\n"+`  "sessions": [],`+"\n"+`  "total": 0`+"\n"+`}` {
		t.Errorf("an empty listing printed %q", out)
	}

	f.seedSession("morning", time.Hour)
	f.seedSession("gone", time.Minute)
	if _, _, err := runWith(t, f.env, "stop", "gone"); err != nil {
		t.Fatal(err)
	}
	var body sessionsBody
	out, _, err = runWith(t, f.env, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	decodeOnly(t, out, &body)
	if len(body.Sessions) != 2 || body.Total != 2 || body.Next != "" {
		t.Errorf("ls --json printed %+v, total %d, next %q", body.Sessions, body.Total, body.Next)
	}
	out, _, err = runWith(t, f.env, "ls", "-n", "1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	body = sessionsBody{}
	decodeOnly(t, out, &body)
	if body.Total != 2 || body.Next == "" {
		t.Errorf("ls -n 1 --json: total %d, next %q; want the cursor to the second", body.Total, body.Next)
	}
}

// @decision(DL-023)
func TestWhoamiJSONIsTheIdentityView(t *testing.T) {
	f := newAPIFixture(t)
	out, _, err := runWith(t, f.env, "whoami", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var me server.MeView
	decodeOnly(t, out, &me)
	if me.UserID == "" || me.OrgID == "" {
		t.Errorf("whoami --json printed %+v", me)
	}
}

// A hosted server's listing is its own body, under the names it stores,
// without the OpenCode line, which is another view.
//
// @decision(DL-023)
func TestCredentialsJSONIsTheServersListing(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "a-forgejo-token", nil }
	if _, _, err := runWith(t, env, "credentials", "add", "forgejo:codefloe.com"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"credentials", "--json"}, {"credentials", "list", "--json"}} {
		out, _, err := runWith(t, env, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var body credentialsBody
		decodeOnly(t, out, &body)
		if !body.Enabled || len(body.Credentials) != 1 || body.Credentials[0].Platform != "forgejo:codefloe.com" {
			t.Errorf("%v printed %+v", args, body)
		}
		if strings.Contains(out, "a-forgejo-token") {
			t.Errorf("%v printed the token", args)
		}
	}

	f = hostedFixture(t)
	out, _, err := runWith(t, f.env, "credentials", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var body credentialsBody
	decodeOnly(t, out, &body)
	if body.Enabled || body.Credentials == nil {
		t.Errorf("a keyless server's listing printed %q", out)
	}
}
