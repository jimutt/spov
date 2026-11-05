package sandbox

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestResolveErrKeepsABadArgumentByName: only a by-ID lookup reads
// InvalidArgument (a malformed ID) as a missing sandbox. By name it is a
// real error, such as an environment that does not exist.
//
// @decision(DL-067)
func TestResolveErrKeepsABadArgumentByName(t *testing.T) {
	p := &ModalProvider{}
	bad := status.Error(codes.InvalidArgument, "environment not found")
	if err := p.resolveErr("spov-x", false, bad); errors.Is(err, ErrNotFound) {
		t.Errorf("by name, InvalidArgument = %v, want a real error", err)
	}
	if err := p.resolveErr("sb-x", true, bad); !errors.Is(err, ErrNotFound) {
		t.Errorf("by ID, InvalidArgument = %v, want ErrNotFound", err)
	}
	for _, byID := range []bool{false, true} {
		if err := p.resolveErr("x", byID, status.Error(codes.NotFound, "no such sandbox")); !errors.Is(err, ErrNotFound) {
			t.Errorf("byID=%v, NotFound = %v, want ErrNotFound", byID, err)
		}
	}
}
