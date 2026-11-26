package store

import "context"

// MaxSeenModelModesBytes bounds what one organisation keeps of the
// reasoning modes its sandboxes reported, as JSON. Each store measures
// its own encoding, so the edge is approximate.
//
// @decision(DL-186)
const MaxSeenModelModesBytes = 1 << 20

// ModelModeStore keeps the newest reasoning modes the organisation's
// sandboxes reported for each model, for one OpenCode version at a time.
//
// @decision(DL-186)
type ModelModeStore interface {
	// Keep lays models over what is kept for (orgID, openCode): each
	// model named replaces its entry, the others stay. What is kept for
	// any other version of the org is deleted. A merge whose JSON would
	// pass MaxSeenModelModesBytes keeps the report alone, and a report
	// past it alone keeps nothing new.
	Keep(ctx context.Context, orgID, openCode string, models map[string][]string) error
	// Seen is what is kept for (orgID, openCode), an empty map when
	// nothing is.
	Seen(ctx context.Context, orgID, openCode string) (map[string][]string, error)
}
