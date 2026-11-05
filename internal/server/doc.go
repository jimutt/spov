// Package server is the spov control plane: the HTTP API — its handlers,
// middleware and OpenAPI document — and the wiring behind `spov serve`.
// It runs in the same binary as the CLI, as a loopback child process in
// local mode or as a hosted service, and is what talks to the sandbox
// provider on the CLI's behalf.
//
// @decision(DL-001)
package server
