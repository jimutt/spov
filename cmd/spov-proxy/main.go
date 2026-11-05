// Command spov-proxy runs in a sandbox's sidecar and adds credentials to
// forge and model provider requests from the main container. It is started with its CA and
// credentials in its environment; `spov-proxy set-token <platform>`
// replaces one platform's token with the one on stdin, and
// `spov-proxy set-key <ENV>` one model provider's key.
//
// @decision(DL-034)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
)

func main() {
	var err error
	if len(os.Args) == 3 && os.Args[1] == "set-token" {
		err = setToken(os.Args[2], os.Stdin)
	} else if len(os.Args) == 3 && os.Args[1] == "set-key" {
		err = setKey(os.Args[2], os.Stdin)
	} else if len(os.Args) == 1 {
		err = serve()
	} else {
		err = errors.New("usage: spov-proxy [set-token <platform> | set-key <ENV>]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "spov-proxy:", err)
		os.Exit(1)
	}
}

func setToken(platform string, stdin io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return err
	}
	return credproxy.SetToken(credproxy.CredentialsFile, platform, string(b))
}

// setKey replaces a model provider's key with the one on stdin.
//
// @decision(DL-015)
func setKey(env string, stdin io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return err
	}
	return credproxy.SetKey(credproxy.CredentialsFile, env, string(b))
}

func serve() error {
	ca, err := credproxy.LoadCA([]byte(os.Getenv(credproxy.EnvCACert)), []byte(os.Getenv(credproxy.EnvCAKey)))
	if err != nil {
		return err
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(os.Getenv(credproxy.EnvConfig)), &cfg); err != nil {
		return fmt.Errorf("%s: %w", credproxy.EnvConfig, err)
	}
	if err := os.MkdirAll(filepath.Dir(credproxy.CredentialsFile), 0o700); err != nil {
		return err
	}
	store, err := credproxy.NewStore(credproxy.CredentialsFile, cfg)
	if err != nil {
		return err
	}
	// Out of this process's environment, so nothing it starts inherits
	// them. Modal still holds them for the sidecar, which the main
	// container cannot see into (spike S-I2).
	for _, k := range []string{credproxy.EnvCAKey, credproxy.EnvConfig} {
		_ = os.Unsetenv(k)
	}
	decrypt := map[string]bool{}
	for _, h := range cfg.DecryptHosts() {
		decrypt[h] = true
	}
	audit := json.NewEncoder(os.Stdout)
	p, err := credproxy.New(credproxy.Config{
		CA:       ca,
		Decrypt:  func(host string) bool { return decrypt[host] },
		Rewrite:  store.Rewrite,
		Secrets:  store.Secrets,
		Streamed: store.Streamed,
		Audit:    func(e credproxy.Entry) { _ = audit.Encode(e) },
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "spov-proxy: "+format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: ":" + strconv.Itoa(credproxy.Port), Handler: p, ReadHeaderTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
