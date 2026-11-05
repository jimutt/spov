package server

import (
	"encoding/json"
	"net/http"
)

// OpenCodePinPath is where a deployment publishes the OpenCode version
// its sandboxes run. Unauthenticated, and on the raw mux rather than in
// huma. Not `opencode.json`, which is OpenCode's own config file inside
// a sandbox.
//
// @decision(DL-035)
const OpenCodePinPath = "/opencode-pin.json"

// PinScheme is the shape of that answer. A client reads it first and
// ignores a body whose scheme it does not know, which is what lets the
// field set grow without a new path.
//
// @decision(DL-035)
const PinScheme = 1

// OpenCodePin is the whole of the answer: the scheme, and the version.
// Nothing else about the deployment belongs here — this is read before
// authentication, by anyone who can reach the port.
//
// @decision(DL-035)
type OpenCodePin struct {
	Scheme   int    `json:"scheme"`
	OpenCode string `json:"opencode"`
}

// handlePin answers the deployment's pin. It reports the configured
// version rather than inspecting an image.
//
// @decision(DL-035)
func (s *Server) handlePin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(OpenCodePin{
		Scheme:   PinScheme,
		OpenCode: s.deps.Defaults.OpenCodeVersion,
	})
}
