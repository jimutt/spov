package git

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFakeListRepos(t *testing.T) {
	f := NewFake()
	repos, err := f.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos = %+v", repos)
	}
	// The returned slice must not alias the fake's own.
	repos[0].Name = "tampered"
	again, _ := f.ListRepos(context.Background())
	if again[0].Name == "tampered" {
		t.Error("ListRepos returned an aliased slice")
	}
}

func TestFakeListReposErrors(t *testing.T) {
	t.Run("forced error", func(t *testing.T) {
		f := NewFake()
		f.ListErr = errors.New("boom")
		if _, err := f.ListRepos(context.Background()); !errors.Is(err, f.ListErr) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("no repos", func(t *testing.T) {
		f := NewFake()
		f.Repos = nil
		if _, err := f.ListRepos(context.Background()); !errors.Is(err, ErrNoRepos) {
			t.Errorf("err = %v, want ErrNoRepos", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := NewFake().ListRepos(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestFakeIssueAndRevoke(t *testing.T) {
	f := NewFake()
	tok, err := f.IssueSandboxToken(context.Background(), f.Repos, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Secret == "" {
		t.Error("no secret was issued")
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("a ttl was given but ExpiresAt is zero")
	}
	if got := f.Requested(); len(got) != 1 || len(got[0]) != 1 {
		t.Errorf("Requested = %+v", got)
	}

	if err := tok.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Revoked() != 1 {
		t.Errorf("Revoked = %d", f.Revoked())
	}
}

func TestFakeIssueWithoutTTL(t *testing.T) {
	f := NewFake()
	tok, err := f.IssueSandboxToken(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Zero ttl mimics Forgejo: no expiry, so revocation is mandatory.
	if !tok.ExpiresAt.IsZero() {
		t.Error("a zero ttl produced an expiry")
	}
}

func TestFakeIssueErrors(t *testing.T) {
	f := NewFake()
	f.IssueErr = errors.New("403")
	if _, err := f.IssueSandboxToken(context.Background(), nil, 0); !errors.Is(err, f.IssueErr) {
		t.Errorf("err = %v", err)
	}
	// The attempt is still recorded so a test can assert on it.
	if len(f.Requested()) != 1 {
		t.Error("the failed attempt was not recorded")
	}

	f2 := NewFake()
	f2.RevokeErr = errors.New("nope")
	tok, err := f2.IssueSandboxToken(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Revoke(context.Background()); !errors.Is(err, f2.RevokeErr) {
		t.Errorf("err = %v", err)
	}
	if f2.Revoked() != 0 {
		t.Error("a failed revocation was counted")
	}
}

func TestFakeIsConcurrencySafe(t *testing.T) {
	f := NewFake()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.ListRepos(context.Background()); err != nil {
				t.Error(err)
			}
			tok, err := f.IssueSandboxToken(context.Background(), f.Repos, time.Hour)
			if err != nil {
				t.Error(err)
				return
			}
			if err := tok.Revoke(context.Background()); err != nil {
				t.Error(err)
			}
			_ = f.Requested()
			_ = f.Revoked()
		}()
	}
	wg.Wait()
	if f.Revoked() != 20 {
		t.Errorf("Revoked = %d, want 20", f.Revoked())
	}
}

func TestFakeSecretsAreDistinct(t *testing.T) {
	f := NewFake()
	seen := map[string]bool{}
	for range 5 {
		tok, err := f.IssueSandboxToken(context.Background(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok.Secret] {
			t.Fatalf("the same secret was issued twice: %q", tok.Secret)
		}
		seen[tok.Secret] = true
	}
}
