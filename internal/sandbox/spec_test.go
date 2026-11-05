package sandbox

import (
	"strings"
	"testing"
	"time"
)

// validSpec returns a Spec that passes Validate, which each test then
// breaks in exactly one way.
func validSpec() Spec {
	return Spec{
		Image:       "spov-opencode2-0.0.0",
		Command:     []string{"bash", "-c", "true"},
		Ports:       []int{4096},
		ReadyPort:   4096,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     4 * time.Hour,
		IdleTimeout: 30 * time.Minute,
		Egress:      EgressOpen(),
	}
}

func TestSpecValidateAccepts(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	t.Run("readiness probe is optional", func(t *testing.T) {
		s := validSpec()
		s.ReadyPort = 0
		if err := s.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})
}

func TestSpecValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"no image", func(s *Spec) { s.Image = "" }, "image"},
		{"no command", func(s *Spec) { s.Command = nil }, "command"},
		{"zero cpu", func(s *Spec) { s.CPU = 0 }, "cpu"},
		{"negative memory", func(s *Spec) { s.MemoryMiB = -1 }, "memory"},
		{"zero timeout", func(s *Spec) { s.Timeout = 0 }, "timeout"},
		{"timeout over the cap", func(s *Spec) { s.Timeout = 13 * time.Hour }, "maximum"},
		{"zero idle timeout", func(s *Spec) { s.IdleTimeout = 0 }, "idle timeout"},
		{"idle timeout over timeout", func(s *Spec) {
			s.Timeout = time.Hour
			s.IdleTimeout = 2 * time.Hour
		}, "exceeds timeout"},
		{"readiness port out of range", func(s *Spec) { s.ReadyPort = 70000 }, "out of range"},
		{"port out of range", func(s *Spec) { s.Ports = []int{0}; s.ReadyPort = 0 }, "out of range"},
		{"duplicate ports", func(s *Spec) { s.Ports = []int{4096, 4096} }, "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			tt.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestSpecTimeoutCapMatchesDecision guards the cost policy: the cap is the
// only thing bounding a leaked sandbox's bill.
func TestSpecTimeoutCapMatchesDecision(t *testing.T) {
	if MaxTimeout != 12*time.Hour {
		t.Errorf("MaxTimeout = %s, want 12h", MaxTimeout)
	}
	s := validSpec()
	s.Timeout = MaxTimeout
	if err := s.Validate(); err != nil {
		t.Errorf("the cap itself must be allowed: %v", err)
	}
	s.Timeout = MaxTimeout + time.Second
	if err := s.Validate(); err == nil {
		t.Error("one second over the cap must be rejected")
	}
}
