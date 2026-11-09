package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// The generated code speaks pgtype; the Store interface speaks time.Time
// and *time.Time. These convert between them in one place.
//
// @decision(DL-003)
func ts(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func tsPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return ts(*t)
}

func goTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

func goTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func sessionFromRow(r sqlcgen.Session) (store.Session, error) {
	s := store.Session{
		ID:        r.ID,
		OrgID:     r.OrgID,
		OwnerID:   r.OwnerID,
		Name:      r.Name,
		State:     store.SessionState(r.State),
		SandboxID: r.SandboxID,
		Landing:   r.Landing,
		Error:     r.Error,
		CreatedAt: goTime(r.CreatedAt),
		UpdatedAt: goTime(r.UpdatedAt),
		StoppedAt: goTimePtr(r.StoppedAt),
		// @decision(DL-003)
		CancelRequestedAt: goTimePtr(r.CancelRequestedAt),
		// @decision(DL-021)
		GitTokenMintedAt:  goTimePtr(r.GitTokenMintedAt),
		GitTokenExpiresAt: goTimePtr(r.GitTokenExpiresAt),
		OpenCodeGrant:     int(r.OpencodeGrant),
	}
	if len(r.Request) > 0 {
		if err := json.Unmarshal(r.Request, &s.Request); err != nil {
			return store.Session{}, fmt.Errorf("store: decode request for session %s: %w", r.ID, err)
		}
	}
	if len(r.Clones) > 0 {
		if err := json.Unmarshal(r.Clones, &s.Clones); err != nil {
			return store.Session{}, fmt.Errorf("store: decode clones for session %s: %w", r.ID, err)
		}
	}
	// @decision(DL-043)
	if len(r.Credentials) > 0 {
		if err := json.Unmarshal(r.Credentials, &s.Credentials); err != nil {
			return store.Session{}, fmt.Errorf("store: decode credentials for session %s: %w", r.ID, err)
		}
	}
	if len(s.Credentials) == 0 {
		s.Credentials = nil
	}
	// @decision(DL-075)
	if len(r.RoutedCredentials) > 0 {
		if err := json.Unmarshal(r.RoutedCredentials, &s.RoutedCredentials); err != nil {
			return store.Session{}, fmt.Errorf("store: decode routed credentials for session %s: %w", r.ID, err)
		}
	}
	if len(s.RoutedCredentials) == 0 {
		s.RoutedCredentials = nil
	}
	// @decision(DL-033)
	if len(r.Booted) > 0 {
		if err := json.Unmarshal(r.Booted, &s.Booted); err != nil {
			return store.Session{}, fmt.Errorf("store: decode booted image for session %s: %w", r.ID, err)
		}
	}
	return s, nil
}

func sessionsFromRows(rows []sqlcgen.Session) ([]store.Session, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.Session, 0, len(rows))
	for _, r := range rows {
		s, err := sessionFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// sessionJSON encodes the jsonb columns. A nil slice must still be a
// JSON array, since the columns are not null.
func sessionJSON(s store.Session) (request, clones, credentials []byte, err error) {
	request, err = json.Marshal(s.Request)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store: encode request: %w", err)
	}
	if s.Clones == nil {
		clones = []byte("[]")
	} else if clones, err = json.Marshal(s.Clones); err != nil {
		return nil, nil, nil, fmt.Errorf("store: encode clones: %w", err)
	}
	// @decision(DL-043)
	if s.Credentials == nil {
		credentials = []byte("[]")
	} else if credentials, err = json.Marshal(s.Credentials); err != nil {
		return nil, nil, nil, fmt.Errorf("store: encode credentials: %w", err)
	}
	return request, clones, credentials, nil
}

// routedJSON encodes the routed_credentials column, never null.
//
// @decision(DL-075)
func routedJSON(s store.Session) ([]byte, error) {
	if s.RoutedCredentials == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(s.RoutedCredentials)
	if err != nil {
		return nil, fmt.Errorf("store: encode routed credentials: %w", err)
	}
	return b, nil
}

// bootedJSON encodes the booted column.
//
// @decision(DL-033)
func bootedJSON(s store.Session) ([]byte, error) {
	b, err := json.Marshal(s.Booted)
	if err != nil {
		return nil, fmt.Errorf("store: encode booted image: %w", err)
	}
	return b, nil
}

func mintFromRow(r sqlcgen.Mint) store.Mint {
	return store.Mint{
		ID:        r.ID,
		OrgID:     r.OrgID,
		SessionID: r.SessionID,
		UserID:    r.UserID,
		Port:      int(r.Port),
		ExpiresAt: goTime(r.ExpiresAt),
		CreatedAt: goTime(r.CreatedAt),
	}
}

func userFromRow(r sqlcgen.User) store.User {
	u := store.User{
		ID:          r.ID,
		OrgID:       r.OrgID,
		Email:       r.Email,
		Name:        r.Name,
		GithubLogin: r.GithubLogin,
		Admin:       r.Admin,
		GitName:     r.GitName,
		GitEmail:    r.GitEmail,
		// @decision(DL-014)
		DefaultModel: r.DefaultModel,
		// @decision(DL-043)
		DefaultGitAccess:    store.GitAccess(r.DefaultGitAccess),
		DefaultJobGitAccess: store.GitAccess(r.DefaultJobGitAccess),
		// @decision(DL-049)
		DefaultJobDuration: time.Duration(r.DefaultJobDurationSeconds) * time.Second,
		LastLoginAt:        goTimePtr(r.LastLoginAt),
		CreatedAt:          goTime(r.CreatedAt),
		UpdatedAt:          goTime(r.UpdatedAt),
	}
	// A quota that will not decode leaves the user on the org defaults
	// rather than making them unreadable: a bad override must not lock
	// someone out of a server they are allowed on.
	//
	// @decision(DL-009)
	if len(r.Quota) > 0 {
		_ = json.Unmarshal(r.Quota, &u.Quota)
	}
	return u
}

// quotaJSON encodes a user's quota for storage. An empty quota is
// stored as {} rather than null, matching the column default.
//
// @decision(DL-009)
func quotaJSON(q store.Quota) ([]byte, error) {
	if q.Empty() {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("store: encode quota: %w", err)
	}
	return b, nil
}

func tokenFromRow(r sqlcgen.ApiToken) store.APIToken {
	return store.APIToken{
		ID:         r.ID,
		OrgID:      r.OrgID,
		UserID:     r.UserID,
		Hash:       r.TokenHash,
		Name:       r.Name,
		LastUsedAt: goTimePtr(r.LastUsedAt),
		ExpiresAt:  goTimePtr(r.ExpiresAt),
		RevokedAt:  goTimePtr(r.RevokedAt),
		CreatedAt:  goTime(r.CreatedAt),
		UpdatedAt:  goTime(r.UpdatedAt),
	}
}

func deviceCodeFromRow(r sqlcgen.DeviceCode) store.DeviceCode {
	return store.DeviceCode{
		ID:              r.ID,
		OrgID:           r.OrgID,
		Hash:            r.DeviceCodeHash,
		UserCode:        r.UserCode,
		UserID:          r.UserID,
		Status:          store.DeviceCodeStatus(r.Status),
		Name:            r.Name,
		ExpiresAt:       goTime(r.ExpiresAt),
		LastPolledAt:    goTimePtr(r.LastPolledAt),
		IntervalSeconds: int(r.IntervalS),
		CreatedAt:       goTime(r.CreatedAt),
		UpdatedAt:       goTime(r.UpdatedAt),
	}
}

func credentialFromRow(r sqlcgen.UserCredential) store.UserCredential {
	return store.UserCredential{
		ID:         r.ID,
		OrgID:      r.OrgID,
		UserID:     r.UserID,
		Platform:   r.Platform,
		Ciphertext: r.Ciphertext,
		Nonce:      r.Nonce,
		KeyVersion: int(r.KeyVersion),
		VerifiedAs: r.VerifiedAs,
		Scopes:     nilIfEmpty(r.Scopes),
		ExpiresAt:  goTime(r.ExpiresAt),
		Route:      r.Route,
		CreatedAt:  goTime(r.CreatedAt),
		UpdatedAt:  goTime(r.UpdatedAt),
	}
}

// mapErr translates pgx and Postgres errors into the store's vocabulary.
// Anything unrecognised is wrapped with what was being done.
//
// @decision(DL-003)
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return store.ErrConflict
		case "23503":
			// A foreign key violation means the row points at
			// something that is not there — a mint at a missing
			// session, or a session at an org that was never seeded.
			// ErrNotFound is right for the caller, but the constraint
			// is what makes the difference debuggable.
			return fmt.Errorf("%w: %s references a row that does not exist", store.ErrNotFound, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

// @decision(DL-017)
func conversationFromRow(r sqlcgen.Conversation) store.Conversation {
	c := store.Conversation{
		ID: r.ID, OrgID: r.OrgID, SessionID: r.SessionID, OpenCodeID: r.OpencodeID, Title: r.Title,
		Info: r.Info, SeqBase: r.SeqBase, SeqAfter: r.SeqAfter, CreatedAt: goTime(r.CreatedAt), UpdatedAt: goTime(r.UpdatedAt),
	}
	if r.ParentOpencodeID != nil {
		c.ParentOpenCodeID = *r.ParentOpencodeID
	}
	return c
}

// nonEmpty is b, or nil when it is empty, so an upsert's coalesce keeps
// what is stored.
func nonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func messageFromRow(r sqlcgen.Message) store.Message {
	return store.Message{
		ID: r.ID, OrgID: r.OrgID, SessionID: r.SessionID, ConversationID: r.ConversationID,
		OpenCodeID: r.OpencodeID, Role: r.Role, Text: r.Text,
		At: goTime(r.At), Raw: r.Raw, CreatedAt: goTime(r.CreatedAt),
	}
}

func eventFromRow(r sqlcgen.Event) store.Event {
	e := store.Event{
		ID: r.ID, OrgID: r.OrgID, SessionID: r.SessionID, Source: store.EventSource(r.Source),
		Pos: r.Pos, Name: r.Name, Text: r.Text, At: goTime(r.At), Raw: r.Raw,
		CreatedAt: goTime(r.CreatedAt),
	}
	// A null seq is a row that has none — a spov marker or an ephemeral
	// frame. The model keeps int64 with 0 meaning "none".
	//
	// @decision(DL-017)
	if r.Seq != nil {
		e.Seq = *r.Seq
	}
	if r.ConversationID != nil {
		e.ConversationID = *r.ConversationID
	}
	return e
}

func jobsFromRows(rows []sqlcgen.Job) []store.Job {
	if len(rows) == 0 {
		return nil
	}
	out := make([]store.Job, 0, len(rows))
	for _, r := range rows {
		out = append(out, jobFromRow(r))
	}
	return out
}

func jobFromRow(r sqlcgen.Job) store.Job {
	return store.Job{
		ID:            r.ID,
		OrgID:         r.OrgID,
		OwnerID:       r.OwnerID,
		SessionID:     r.SessionID,
		Kind:          store.JobKind(r.Kind),
		State:         store.JobState(r.State),
		Goal:          r.Goal,
		CheckCommand:  r.CheckCommand,
		CheckProposed: r.CheckProposed,
		Assessment:    r.Assessment,
		Model:         r.Model,
		Verdict:       r.Verdict,
		Unfinished:    r.Unfinished,
		Error:         r.Error,
		Attempt:       int(r.Attempt),
		Cursor:        r.Cursor,
		CreatedAt:     goTime(r.CreatedAt),
		UpdatedAt:     goTime(r.UpdatedAt),
		StartedAt:     goTimePtr(r.StartedAt),
		StoppedAt:     goTimePtr(r.StoppedAt),
		// @decision(DL-049)
		MaxDuration:    time.Duration(r.MaxDurationSeconds) * time.Second,
		ClockStartedAt: goTimePtr(r.ClockStartedAt),
		// @decision(DL-049)
		ClockUsed: time.Duration(r.ClockUsedSeconds) * time.Second,
		// @decision(DL-102)
		SyncAppliedAt: goTimePtr(r.SyncAppliedAt),
	}
}

// deliverableFromRow converts a stored member of a job's inventory.
//
// @decision(DL-051)
func deliverableFromRow(r sqlcgen.JobDeliverable) store.Deliverable {
	return store.Deliverable{
		JobID:      r.JobID,
		Key:        r.Key,
		Kind:       store.DeliverableKind(r.Kind),
		Status:     store.DeliverableStatus(r.Status),
		Detail:     r.Detail,
		Platform:   r.Platform,
		Forge:      r.Forge,
		ImagesLost: r.ImagesLost,
		Owner:      r.Owner,
		Name:       r.Name,
		Branch:     r.Branch,
		Number:     int(r.Number),
		URL:        r.Url,
		Title:      r.Title,
		Path:       r.Path,
		Size:       r.Size,
		HeadCommit: r.HeadCommit,
		Commits:    int(r.Commits),
		Files:      int(r.Files),
		CreatedAt:  goTime(r.CreatedAt),
		UpdatedAt:  goTime(r.UpdatedAt),
	}
}

// nonNilStrings is a list for a `text[] not null` column, where nil
// would be written as NULL.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nilIfEmpty reads an empty array column back as nil, as the memory
// store holds it.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
