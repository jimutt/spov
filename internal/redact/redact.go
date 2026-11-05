// Package redact is DL-005's credential filter: the one place that
// decides what a leaked secret looks like. It is its own package,
// separate from internal/server and internal/jobs, so the two sides of
// DL-046's compiler-enforced wall share one filter rather than two that
// can drift.
//
// @decision(DL-005) @decision(DL-046)
package redact

import (
	"regexp"
	"strings"
)

// secretPatterns are shapes a credential takes when it leaks into an
// error message: URL userinfo, token-bearing query parameters, HTTP auth
// headers, and the prefixes Modal, GitHub and Forgejo tokens carry. Long
// opaque strings are redacted as well, since a leak is worse than a
// terse message.
//
// @decision(DL-005)
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`),
	regexp.MustCompile(`(?i)([?&](?:token|access_token|password|secret|key|api_key|apikey)=)[^&\s"']*`),
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+/=]+`),
	regexp.MustCompile(`\b(ak|as)-[A-Za-z0-9]{8,}\b`),
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`(?i)(?:password|passwd|secret|token|api[_-]?key)["']?\s*[:=]\s*["']?[^\s"',;}]+`),
	regexp.MustCompile(`\b[A-Za-z0-9_-]{40,}\b`),
}

// Error returns an error's text with anything that looks like a
// credential replaced. Every message that crosses the API boundary or is
// stored goes through it.
//
// @decision(DL-005)
func Error(err error) string {
	if err == nil {
		return ""
	}
	return Text(err.Error())
}

// Text is Error for a string that is not an error.
//
// @decision(DL-005) @decision(DL-008)
func Text(msg string) string {
	for i, re := range secretPatterns {
		switch i {
		case 0, 1, 2, 3:
			msg = re.ReplaceAllString(msg, "${1}[redacted]")
		case len(secretPatterns) - 2:
			// Keep the label so the message still says what failed.
			msg = re.ReplaceAllStringFunc(msg, func(m string) string {
				for j, r := range m {
					if r == ':' || r == '=' {
						return m[:j+1] + "[redacted]"
					}
				}
				return "[redacted]"
			})
		default:
			msg = re.ReplaceAllString(msg, "[redacted]")
		}
	}
	return strings.TrimSpace(msg)
}
