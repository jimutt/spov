package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	return encodeJSON(w, v, "  ")
}

// encodeJSON writes v as JSON with the characters encoding/json leaves
// raw but a terminal acts on (C1 controls, bidirectional overrides)
// written as \u escapes, so a pipe to a terminal is as safe as a read
// with jq. Control characters below U+0020 are escaped by the encoder.
//
// @decision(DL-136)
func encodeJSON(w io.Writer, v any, indent string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(escapeJSONText(buf.Bytes()))
	return err
}

// escapeJSONText rewrites every raw C1 control, bidirectional control
// and DEL in an encoded document as a \u escape. They occur only inside
// strings, where the escape means the same character.
func escapeJSONText(b []byte) []byte {
	if !bytes.ContainsFunc(b, func(r rune) bool { return r == 0x7f || (r >= 0x80 && r <= 0x9f) || isFormatSpoof(r) }) {
		return b
	}
	var out bytes.Buffer
	for _, r := range string(b) {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || isFormatSpoof(r) {
			fmt.Fprintf(&out, "\\u%04x", r)
			continue
		}
		out.WriteRune(r)
	}
	return out.Bytes()
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
