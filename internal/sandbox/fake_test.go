package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeCreateRecordsSpec(t *testing.T) {
	f := NewFake()
	spec := validSpec()
	spec.Name = "morning"
	spec.Tags = BuildTags(TagSet{Name: "morning", User: "alice", CreatedAt: time.Now()})

	h, err := f.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Created) != 1 {
		t.Fatalf("Created = %v", f.Created)
	}
	if f.Created[0].Image != spec.Image {
		t.Errorf("recorded spec = %+v", f.Created[0])
	}

	info, err := h.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateRunning {
		t.Errorf("state = %q, want running", info.State)
	}
	if info.Name != "morning" {
		t.Errorf("name = %q", info.Name)
	}
}

func TestFakeCreateValidatesSpec(t *testing.T) {
	f := NewFake()
	bad := validSpec()
	bad.Timeout = 99 * time.Hour

	if _, err := f.Create(context.Background(), bad); err == nil {
		t.Fatal("the fake must apply the same validation as the real provider")
	}
	if len(f.Created) != 0 {
		t.Errorf("an invalid spec was recorded as created: %v", f.Created)
	}
}

func TestFakeCreateError(t *testing.T) {
	f := NewFake()
	f.CreateErr = errors.New("boom")

	_, err := f.Create(context.Background(), validSpec())
	if !errors.Is(err, f.CreateErr) {
		t.Fatalf("err = %v", err)
	}
	// The attempt is still recorded, so a test can assert on what spov
	// tried to create even when creation fails.
	if len(f.Created) != 1 {
		t.Errorf("Created = %v, want the failed attempt recorded", f.Created)
	}
}

func TestFakeTunnels(t *testing.T) {
	f := NewFake()
	spec := validSpec()
	spec.Ports = []int{4096, 8080}
	spec.ReadyPort = 4096

	h, err := f.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	tunnels, err := h.Tunnels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tunnels) != 2 {
		t.Fatalf("tunnels = %v", tunnels)
	}
	for _, port := range spec.Ports {
		if tunnels[port] == "" {
			t.Errorf("no tunnel for port %d", port)
		}
	}

	// Mutating the returned map must not affect the handle.
	tunnels[4096] = "mutated"
	again, _ := h.Tunnels(context.Background())
	if again[4096] == "mutated" {
		t.Error("Tunnels returned an aliased map")
	}
}

func TestFakeGet(t *testing.T) {
	f := NewFake()
	spec := validSpec()
	spec.Name = "morning"
	h, err := f.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(context.Background())

	t.Run("by id", func(t *testing.T) {
		got, err := f.Get(context.Background(), info.ID)
		if err != nil {
			t.Fatal(err)
		}
		gotInfo, _ := got.Info(context.Background())
		if gotInfo.ID != info.ID {
			t.Errorf("id = %q, want %q", gotInfo.ID, info.ID)
		}
	})

	t.Run("by name", func(t *testing.T) {
		got, err := f.Get(context.Background(), "morning")
		if err != nil {
			t.Fatal(err)
		}
		gotInfo, _ := got.Info(context.Background())
		if gotInfo.ID != info.ID {
			t.Errorf("id = %q, want %q", gotInfo.ID, info.ID)
		}
	})

	t.Run("unknown reference", func(t *testing.T) {
		_, err := f.Get(context.Background(), "nope")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestFakeListFiltersByTags(t *testing.T) {
	f := NewFake()
	base := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return base }

	mine := validSpec()
	mine.Name = "mine"
	mine.Tags = BuildTags(TagSet{Name: "mine", User: "alice", CreatedAt: base})
	if _, err := f.Create(context.Background(), mine); err != nil {
		t.Fatal(err)
	}

	f.Now = func() time.Time { return base.Add(time.Hour) }
	other := validSpec()
	other.Name = "other"
	other.Tags = map[string]string{"someone-else": "1"}
	if _, err := f.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	owned, err := f.List(context.Background(), OwnedTags())
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].Name != "mine" {
		t.Fatalf("owned = %+v, want only the spov-tagged sandbox", owned)
	}

	all, err := f.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %+v", all)
	}
	// Newest first, so `spov ls` shows recent sessions at the top.
	if all[0].Name != "other" {
		t.Errorf("list is not newest-first: %+v", all)
	}
}

func TestFakeTerminate(t *testing.T) {
	f := NewFake()
	h, err := f.Create(context.Background(), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	if f.Running() != 1 {
		t.Fatalf("Running = %d, want 1", f.Running())
	}

	if err := h.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.(*FakeHandle).WasTerminated() {
		t.Error("Terminated flag not set")
	}
	if f.Running() != 0 {
		t.Errorf("Running = %d after terminate, want 0", f.Running())
	}

	info, _ := h.Info(context.Background())
	if info.State != StateStopped {
		t.Errorf("state = %q, want stopped", info.State)
	}
	if info.ExitCode == nil {
		t.Error("a stopped sandbox must report an exit code")
	}
}

func TestFakeTerminateError(t *testing.T) {
	f := NewFake()
	f.TerminateErr = errors.New("nope")
	h, err := f.Create(context.Background(), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Terminate(context.Background()); !errors.Is(err, f.TerminateErr) {
		t.Fatalf("err = %v", err)
	}
	if f.Running() != 1 {
		t.Error("a failed terminate must leave the sandbox running")
	}
}

func TestFakeExec(t *testing.T) {
	f := NewFake()
	f.ExecResults["echo hi"] = ExecResult{ExitCode: 0, Stdout: "hi\n"}
	h, err := f.Create(context.Background(), validSpec())
	if err != nil {
		t.Fatal(err)
	}

	res, err := h.Exec(execCtx(t), []string{"echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}

	if _, err := h.Exec(execCtx(t), []string{"unmatched"}); err != nil {
		t.Fatalf("an unmatched command should return a zero result, not an error: %v", err)
	}
	if got := h.(*FakeHandle).ExecLog(); len(got) != 2 {
		t.Errorf("Execs = %v, want both commands recorded", got)
	}
}

func TestFakeDetach(t *testing.T) {
	f := NewFake()
	h, err := f.Create(context.Background(), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Detach(); err != nil {
		t.Fatal(err)
	}
	if !h.(*FakeHandle).WasDetached() {
		t.Error("Detached flag not set")
	}
	// Detaching must not stop the sandbox: that is the --keep path.
	if f.Running() != 1 {
		t.Error("Detach terminated the sandbox")
	}
}

func TestFakeInfoDoesNotAliasTags(t *testing.T) {
	f := NewFake()
	spec := validSpec()
	spec.Tags = BuildTags(TagSet{Name: "s", User: "u", CreatedAt: time.Now()})
	h, err := f.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(context.Background())
	info.Tags["spov"] = "tampered"

	again, _ := h.Info(context.Background())
	if again.Tags["spov"] != "1" {
		t.Error("Info returned an aliased tag map")
	}
}

func TestFakeRejectsDuplicateNames(t *testing.T) {
	f := NewFake()
	spec := validSpec()
	spec.Name = "morning"

	if _, err := f.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// Modal names are unique within an app; the fake must not let a test
	// pass that the real provider would reject.
	if _, err := f.Create(context.Background(), spec); err == nil {
		t.Fatal("a duplicate name was accepted")
	}

	h, err := f.Get(context.Background(), "morning")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(context.Background())
	if info.ID != "sb-fake-1" {
		t.Errorf("the original sandbox was shadowed: %s", info.ID)
	}
}

func TestFakeRejectsUseAfterDetach(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(Handle) error
	}{
		{"detached", func(h Handle) error { return h.Detach() }},
		{"terminated", func(h Handle) error { return h.Terminate(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake()
			h, err := f.Create(context.Background(), validSpec())
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.stop(h); err != nil {
				t.Fatal(err)
			}

			// The real provider rejects every operation once the handle is
			// detached, and Terminate detaches internally.
			if _, err := h.Tunnels(context.Background()); err == nil {
				t.Error("Tunnels succeeded on an unusable handle")
			}
			if _, err := h.Exec(execCtx(t), []string{"echo"}); err == nil {
				t.Error("Exec succeeded on an unusable handle")
			}
			// Info stays available: `spov ls` reports terminated sandboxes.
			if _, err := h.Info(context.Background()); err != nil {
				t.Errorf("Info must keep working: %v", err)
			}
		})
	}
}
