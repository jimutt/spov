package server

import (
	"strings"
	"testing"
)

// A GitLab token's launch line says GraphQL is not narrowed, as a GitHub
// PAT's does.
//
// @decision(DL-034)
func TestTheProxiedNoteNamesGitLabsGraphQL(t *testing.T) {
	if got := ProxiedTokenNote("gl", "gitlab", false); !strings.Contains(got, "GitLab's GraphQL") {
		t.Errorf("note = %q", got)
	}
	if got := ProxiedTokenNote("gt", "gitea", false); strings.Contains(got, "GraphQL") {
		t.Errorf("gitea note = %q", got)
	}
}
