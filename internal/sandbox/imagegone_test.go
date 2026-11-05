package sandbox

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestImageGoneIsOnlyAMissingImage: the create of a deleted image fails
// with NotFound naming the image (spike DL-016); nothing else counts.
//
// @decision(DL-016)
func TestImageGoneIsOnlyAMissingImage(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{status.Error(codes.NotFound, "Image im-abc not found"), true},
		{status.Error(codes.NotFound, "Secret spov-x not found"), false},
		{status.Error(codes.InvalidArgument, "image is invalid"), false},
		{errors.New("image not found"), false},
	} {
		if got := imageGone(c.err); got != c.want {
			t.Errorf("imageGone(%v) = %v", c.err, got)
		}
	}
}
