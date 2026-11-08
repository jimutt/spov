package store

import "testing"

// The person's conversation is the oldest root, whatever order the
// subagents were seen in, and the oldest of all when no root is known.
//
// @decision(DL-110)
func TestPrimaryIsTheOldestRoot(t *testing.T) {
	child := Conversation{ID: "c", OpenCodeID: "ses_child", ParentOpenCodeID: "ses_root"}
	root := Conversation{ID: "r", OpenCodeID: "ses_root"}
	later := Conversation{ID: "l", OpenCodeID: "ses_later"}
	for _, c := range []struct {
		convs []Conversation
		want  string
		ok    bool
	}{
		{[]Conversation{child, root, later}, "r", true},
		{[]Conversation{root, child}, "r", true},
		{[]Conversation{child}, "c", true},
		{nil, "", false},
	} {
		got, ok := Primary(c.convs)
		if got.ID != c.want || ok != c.ok {
			t.Errorf("Primary(%v) = %q, %v; want %q, %v", c.convs, got.ID, ok, c.want, c.ok)
		}
	}
}
