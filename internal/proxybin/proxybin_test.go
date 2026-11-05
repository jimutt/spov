package proxybin

import (
	"testing"
	"testing/fstest"
)

// A build without the binary says so, and a build with it returns it.
//
// @decision(DL-034)
func TestTheBinaryIsThereOnlyWhenBuiltIn(t *testing.T) {
	fsys := fstest.MapFS{
		"dist/.gitkeep": {},
		"built/" + File: {Data: []byte("ELF")},
		"empty/" + File: {},
	}
	if b := binaryFrom(fsys, "dist/"+File); b != nil {
		t.Errorf("the placeholder gave %d bytes", len(b))
	}
	if b := binaryFrom(fsys, "empty/"+File); b != nil {
		t.Errorf("an empty file gave %d bytes", len(b))
	}
	if b := binaryFrom(fsys, "built/"+File); string(b) != "ELF" {
		t.Errorf("the built binary gave %q", b)
	}
	_ = Binary()
	if got := Hash([]byte("x")); len(got) != 64 || got == Hash([]byte("y")) {
		t.Errorf("Hash = %q", got)
	}
}
