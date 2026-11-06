package cli

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
)

// --json prints the API's own view for the call a command makes, and
// nothing else on stdout. Tables and prose are for people.
//
// @decision(DL-023)

// jsonFlag registers --json on cmd.
//
// @decision(DL-023)
func jsonFlag(cmd *cobra.Command, on *bool) {
	cmd.Flags().BoolVar(on, "json", false, "print the API's JSON view instead, and nothing else on stdout")
}

// writeJSON prints v indented, with <, > and & as written.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// The listings' bodies as the API sends them, so a script reads the
// shape the OpenAPI document describes. A nil list is printed as [].
//
// @decision(DL-023)
type (
	// The listing bodies carry the page's cursor and total, as the API
	// sends them. @decision(DL-012)
	sessionsBody struct {
		Sessions []server.SessionView `json:"sessions"`
		Next     string               `json:"next,omitempty"`
		Total    int                  `json:"total"`
	}
	jobsBody struct {
		Jobs  []server.JobView `json:"jobs"`
		Next  string           `json:"next,omitempty"`
		Total int              `json:"total"`
	}
	credentialsBody struct {
		Credentials []server.CredentialView `json:"credentials"`
		Enabled     bool                    `json:"enabled"`
		// @decision(DL-073)
		RoutedCredentials []server.RoutedCredentialView `json:"routed_credentials"`
		// @decision(DL-074)
		OrgRoutedCredentials []server.RoutedCredentialView `json:"org_routed_credentials,omitempty"`
	}
)

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
