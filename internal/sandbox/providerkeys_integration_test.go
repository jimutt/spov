//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/prep"
)

// TestIntegrationSKModelCallsThroughTheProxy is spike S-K1: a model call
// through the credential proxy for every catalogue provider, at the
// pinned build. The main container holds a placeholder for each key and
// its own egress is closed, so a call that reaches its provider went
// through the sidecar. The keys the proxy sets are not valid either, so
// no key is needed: an audit entry for the provider's host, whatever its
// status, shows the route, the trust and that the proxy set the key.
//
// @decision(DL-034)
func TestIntegrationSKModelCallsThroughTheProxy(t *testing.T) {
	bin := buildProxy(t)
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	var cfg credproxy.SidecarConfig
	env := credproxy.MainEnv()
	for _, prov := range prep.Catalogue() {
		// Providers that share a key variable share its one entry.
		if _, ok := env[prov.KeyEnv]; ok {
			continue
		}
		key := "spov-spike-not-a-key-" + strings.ToLower(prov.ID)
		cfg.Providers = append(cfg.Providers, credproxy.Provider{Env: prov.KeyEnv, ProviderAPI: prov.API, Key: key})
		ph, err := credproxy.KeyPlaceholder(key)
		if err != nil {
			t.Fatal(err)
		}
		env[prov.KeyEnv] = ph
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	caCert, caKey, err := credproxy.NewCA(cfg.CAHosts(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(cfg)
	sb, creds, password := startOpenCodeSandboxEnv(t, ctx, p, "spov-sk1", 15*time.Minute,
		map[string]string{credproxy.MainCAFile: string(caCert)}, env)

	image, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := sb.ExperimentalSidecars.Create(ctx, credproxy.SidecarName, image, &modal.SidecarCreateParams{
		Command: []string{"sleep", "infinity"},
		Env: map[string]string{
			credproxy.EnvCACert: string(caCert), credproxy.EnvCAKey: string(caKey), credproxy.EnvConfig: string(config),
		},
	})
	if err != nil {
		t.Fatalf("sidecar create: %v", err)
	}
	if code, out, errOut := runIn(ctx, t, proxy, "cat > "+credproxy.BinaryPath+" && chmod 755 "+credproxy.BinaryPath, bin); code != 0 {
		t.Fatalf("upload: exit %d %s%s", code, out, errOut)
	}
	if code, out, errOut := runIn(ctx, t, proxy, "setsid "+credproxy.BinaryPath+" >/tmp/proxy.log 2>&1 < /dev/null &", nil); code != 0 {
		t.Fatalf("start: exit %d %s%s", code, out, errOut)
	}
	if code, _, errOut := execIn(ctx, t, sb, "bash", "-c",
		"for i in $(seq 100); do (exec 3<>/dev/tcp/proxy/3128) 2>/dev/null && exit 0; sleep 0.1; done; exit 1"); code != 0 {
		t.Fatalf("the proxy never answered: %s", errOut)
	}
	if code, _, errOut := execIn(ctx, t, sb, "bash", "-c", "cat "+credproxy.MainCAFile+" >> "+credproxy.SystemBundle); code != 0 {
		t.Fatalf("trust the CA: %s", errOut)
	}
	// With main's own egress closed, a call that reaches a provider went
	// through the proxy.
	if err := sb.UpdateNetworkPolicy(ctx, &modal.SandboxUpdateNetworkPolicyParams{
		OutboundCIDRAllowlist: &modal.Allowlist{}, OutboundDomainAllowlist: &modal.Allowlist{},
	}); err != nil {
		t.Fatalf("close main's egress: %v", err)
	}
	time.Sleep(3 * time.Second)

	client := &http.Client{Timeout: 60 * time.Second}
	audited := func(host string) (credproxy.Entry, bool) {
		_, log, _ := runIn(ctx, t, proxy, "cat /tmp/proxy.log", nil)
		for _, line := range strings.Split(log, "\n") {
			var e credproxy.Entry
			if json.Unmarshal([]byte(line), &e) == nil && e.Host == host {
				return e, true
			}
		}
		return credproxy.Entry{}, false
	}
	// One provider at a time, so a miss is that provider's. A model
	// OpenCode's resolver calls unavailable ends the turn before any call,
	// so the provider's next model is tried.
	list := modelcat.Snapshot()
	for _, prov := range prep.Catalogue() {
		models := modelIDs(list, prov.ID)
		var (
			e     credproxy.Entry
			ok    bool
			model string
			took  time.Duration
		)
		for _, model = range models[:min(3, len(models))] {
			sess := createSession(ctx, t, client, creds.URL, creds.Token, password)
			post(ctx, t, client, creds.URL+"/api/session/"+sess+"/model", creds.Token, password,
				fmt.Sprintf(`{"model":{"id":%q,"providerID":%q}}`, model, prov.ID))
			start := time.Now()
			prompt(ctx, t, client, creds.URL, creds.Token, password, sess, "Say hi.")
			for time.Since(start) < 45*time.Second {
				if e, ok = audited(prov.API.Host); ok {
					break
				}
				time.Sleep(2 * time.Second)
			}
			took = time.Since(start)
			if ok {
				break
			}
			_, unavailable, _ := execIn(ctx, t, sb, "bash", "-c",
				"grep -rho 'Model unavailable: "+prov.ID+"/"+model+"' /root/.local/share/opencode*/log 2>/dev/null | wc -l")
			t.Logf("RESULT %-10s %s made no call through the proxy (OpenCode's 'Model unavailable' lines: %s)",
				prov.ID, model, strings.TrimSpace(unavailable))
		}
		if !ok {
			t.Errorf("RESULT %-10s no call reached %s through the proxy: OpenCode's provider ignored HTTPS_PROXY, "+
				"did not trust the CA, or calls another host", prov.ID, prov.API.Host)
			continue
		}
		t.Logf("RESULT %-10s %s: %s %s%s -> %d, %s after the prompt", prov.ID, model, e.Method, e.Host, e.Path,
			e.Status, took.Round(time.Second))
	}
	_, log, _ := runIn(ctx, t, proxy, "cat /tmp/proxy.log", nil)
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "spov-proxy:") {
			t.Logf("RESULT proxy said: %s", line)
		}
	}

	// GA's credential endpoint returns what OpenCode holds. It must hold
	// no real key, only what the main container was given.
	//
	// @decision(DL-035) @decision(DL-034)
	code, stored := steerCall(ctx, t, client, http.MethodGet, creds.URL+"/api/credential", creds.Token, password, "")
	if code != http.StatusOK {
		t.Errorf("GET /api/credential answered %d, so the store was not read: %s", code, truncate(stored, 300))
	}
	if strings.Contains(stored, "spov-spike-not-a-key-") {
		t.Errorf("OpenCode's credential store holds a key the sidecar was given: %s", truncate(stored, 400))
	}
	t.Logf("RESULT /api/credential answered: %s", truncate(stored, 300))
}

// TestIntegrationSKARunningOpenCodeAndANewCA is spike S-K2: what a
// running OpenCode trusts when its proxy restarts. With the sandbox's own
// CA the running session must keep working; with a new CA, installed by
// credproxy.InstallCACommand, the results are logged. The proxy is
// stopped by its pid, since the image has no pkill.
//
// @decision(DL-034)
func TestIntegrationSKARunningOpenCodeAndANewCA(t *testing.T) {
	bin := buildProxy(t)
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// SPOV_SK2_PROVIDER picks the provider (anthropic, the default, or
	// openrouter): each OpenCode provider builds its own client.
	provider, keyEnv, placeholder := "anthropic", "ANTHROPIC_API_KEY", "sk-ant-api03-spovplaceholder"
	if os.Getenv("SPOV_SK2_PROVIDER") == "openrouter" {
		provider, keyEnv, placeholder = "openrouter", "OPENROUTER_API_KEY", "sk-or-v1-spovplaceholder"
	}
	api := prep.ProviderAPIs()[keyEnv]
	cfg := credproxy.SidecarConfig{Providers: []credproxy.Provider{{
		Env: keyEnv, ProviderAPI: api, Key: "spov-spike-not-a-key",
	}}}
	config, _ := json.Marshal(cfg)
	caCert, caKey, err := credproxy.NewCA(cfg.CAHosts(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env := credproxy.MainEnv()
	env[keyEnv] = placeholder
	sb, creds, password := startOpenCodeSandboxEnv(t, ctx, p, "spov-sk2", 12*time.Minute,
		map[string]string{credproxy.MainCAFile: string(caCert)}, env)
	image, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := sb.ExperimentalSidecars.Create(ctx, credproxy.SidecarName, image, &modal.SidecarCreateParams{
		Command: []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("sidecar create: %v", err)
	}
	if code, out, errOut := runIn(ctx, t, proxy, "cat > "+credproxy.BinaryPath+" && chmod 755 "+credproxy.BinaryPath, bin); code != 0 {
		t.Fatalf("upload: exit %d %s%s", code, out, errOut)
	}
	if code, _, errOut := runIn(ctx, t, proxy, "cat > /tmp/config.json", config); code != 0 {
		t.Fatalf("config: %s", errOut)
	}
	// startProxy runs the proxy with a CA, as a new sidecar would start.
	startProxy := func(cert, key []byte) {
		t.Helper()
		if code, _, errOut := runIn(ctx, t, proxy, "cat > /tmp/ca.crt", cert); code != 0 {
			t.Fatalf("cert: %s", errOut)
		}
		if code, _, errOut := runIn(ctx, t, proxy, "cat > /tmp/ca.key", key); code != 0 {
			t.Fatalf("key: %s", errOut)
		}
		// The image has no pkill: the proxy is stopped by its recorded
		// pid, or the old one, with the old CA, keeps serving.
		script := "[ -f /tmp/proxy.pid ] && kill $(cat /tmp/proxy.pid) && sleep 1; " +
			credproxy.EnvCACert + `="$(cat /tmp/ca.crt)" ` + credproxy.EnvCAKey + `="$(cat /tmp/ca.key)" ` +
			credproxy.EnvConfig + `="$(cat /tmp/config.json)" setsid ` + credproxy.BinaryPath +
			" >>/tmp/proxy.log 2>&1 < /dev/null & echo $! > /tmp/proxy.pid"
		if code, out, errOut := runIn(ctx, t, proxy, script, nil); code != 0 {
			t.Fatalf("start: exit %d %s%s", code, out, errOut)
		}
		if code, _, errOut := execIn(ctx, t, sb, "bash", "-c",
			"sleep 1; for i in $(seq 100); do (exec 3<>/dev/tcp/proxy/3128) 2>/dev/null && exit 0; sleep 0.1; done; exit 1"); code != 0 {
			t.Fatalf("the proxy never answered: %s", errOut)
		}
	}
	startProxy(caCert, caKey)
	if code, _, errOut := execIn(ctx, t, sb, "bash", "-c", "cat "+credproxy.MainCAFile+" >> "+credproxy.SystemBundle); code != 0 {
		t.Fatalf("trust the CA: %s", errOut)
	}
	if err := sb.UpdateNetworkPolicy(ctx, &modal.SandboxUpdateNetworkPolicyParams{
		OutboundCIDRAllowlist: &modal.Allowlist{}, OutboundDomainAllowlist: &modal.Allowlist{},
	}); err != nil {
		t.Fatalf("close main's egress: %v", err)
	}
	time.Sleep(3 * time.Second)

	client := &http.Client{Timeout: 60 * time.Second}
	calls := func() int {
		_, log, _ := runIn(ctx, t, proxy, "cat /tmp/proxy.log", nil)
		return strings.Count(log, `"Host":"`+api.Host+`"`)
	}
	// call prompts sess, or a new session when sess is "", and reports
	// whether the call reached the provider, with the session it used.
	call := func(label, sess string) (bool, string) {
		t.Helper()
		before := calls()
		if sess == "" {
			sess = createSession(ctx, t, client, creds.URL, creds.Token, password)
			post(ctx, t, client, creds.URL+"/api/session/"+sess+"/model", creds.Token, password,
				fmt.Sprintf(`{"model":{"id":%q,"providerID":%q}}`, modelIDs(modelcat.Snapshot(), provider)[0], provider))
		}
		prompt(ctx, t, client, creds.URL, creds.Token, password, sess, "Say hi.")
		for start := time.Now(); time.Since(start) < 45*time.Second; time.Sleep(2 * time.Second) {
			if calls() > before {
				t.Logf("RESULT %s: the call reached the provider through the proxy", label)
				return true, sess
			}
		}
		_, msgs := getCode(ctx, t, client, creds.URL+"/api/session/"+sess+"/message", creds.Token, password)
		t.Logf("RESULT %s: no call reached the provider; the session holds: %s", label, truncate(msgs, 1500))
		return false, sess
	}
	ok, first := call("with the first CA", "")
	if !ok {
		// The first call after a sandbox starts has been seen to miss once.
		if ok, first = call("with the first CA, again", ""); !ok {
			t.Fatal("the baseline call did not go through")
		}
	}

	// A recreated sidecar that keeps the sandbox's CA (DL-034): the proxy
	// process restarted with the same certificate and key. The running
	// session must keep working, with nothing installed in the main
	// container.
	startProxy(caCert, caKey)
	if ok, _ := call("the same session after a restart with the same CA", first); !ok {
		t.Error("a sidecar restarted with the sandbox's own CA broke the running session")
	}

	// A new CA, as DL-034 recreated with before DL-034: measured, since
	// it is what a recreation still falls back to.
	cert2, key2, err := credproxy.NewCA(cfg.CAHosts(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	startProxy(cert2, key2)
	res, err := (&modalHandle{sb: sb}).ExecInput(ctx, credproxy.InstallCACommand(), cert2)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("install the new CA: %v %+v", err, res)
	}
	if res, err := (&modalHandle{sb: sb}).ExecInput(ctx, []string{"sh", "-c", "cat > /tmp/ca2.crt"}, cert2); err != nil || res.ExitCode != 0 {
		t.Fatalf("write the new CA: %v %+v", err, res)
	}
	// The proxy now serves leaves of the new CA alone: verified against
	// the new certificate only, not the bundle that trusts both.
	if code, _, _ := execIn(ctx, t, sb, "bash", "-c", "echo | openssl s_client -proxy proxy:3128 -connect "+api.Host+":443 -servername "+api.Host+
		" -CAfile /tmp/ca2.crt -verify_return_error >/dev/null 2>&1"); code != 0 {
		t.Fatal("the proxy does not serve the new CA's leaves, so nothing below measures a new CA")
	}
	_, curl, _ := execIn(ctx, t, sb, "bash", "-c", "curl -sS -o /dev/null -w '%{http_code}' https://"+api.Host+"/ 2>&1")
	t.Logf("RESULT a new process (curl) after the new CA: %s", curl)
	call("the same session after a new CA", first)
	call("a new session after a new CA, same OpenCode process", "")
	code, out := post(ctx, t, client, creds.URL+"/api/location/reload", creds.Token, password, "{}")
	t.Logf("RESULT POST /api/location/reload: %d %s", code, truncate(out, 300))
	call("the same session after a new CA and a location reload", first)
}

// modelIDs are a provider's model ids in the list, newest first.
func modelIDs(l modelcat.List, provider string) []string {
	var out []string
	for _, m := range l.Providers[provider] {
		out = append(out, m.ID)
	}
	return out
}
