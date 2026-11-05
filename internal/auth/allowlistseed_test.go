package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// writeAllowlist writes content to a temporary file and returns its path.
func writeAllowlist(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "allowlist")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seed imports content into a fresh store and returns the list built
// over it, which is the pair a hosted server actually starts with.
func seed(t *testing.T, content string) (*store.Memory, *Allowlist, error) {
	t.Helper()
	return seedPath(t, writeAllowlist(t, content))
}

func seedPath(t *testing.T, path string) (*store.Memory, *Allowlist, error) {
	t.Helper()
	st := store.NewMemory()
	if _, err := SeedAllowlist(context.Background(), st, "org", path, nil); err != nil {
		return st, nil, err
	}
	list, err := NewAllowlist(context.Background(), StoreAllowlist(st.Allowlist(), "org"), AllowlistRefresh, nil)
	return st, list, err
}

// The seed keeps DL-007's parse rules exactly, so a file written for the
// old server bootstraps the new one.
//
// @decision(DL-007)
func TestTheSeedParsesEveryLineShape(t *testing.T) {
	_, list, err := seed(t, `
# The people who may use this server.
alice@example.com
BOB@Example.COM   admin

  carol@example.com  # trailing comment
dave@example.com ADMIN
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		email string
		admin bool
	}{
		{"alice@example.com", false},
		{"bob@example.com", true},
		// The lookup is case-insensitive in both directions.
		{"Alice@Example.com", false},
		{"carol@example.com", false},
		// "admin" is matched regardless of case.
		{"dave@example.com", true},
	} {
		admin, ok := list.Lookup(tc.email)
		if !ok {
			t.Errorf("%s is not allowed", tc.email)
			continue
		}
		if admin != tc.admin {
			t.Errorf("%s admin = %v, want %v", tc.email, admin, tc.admin)
		}
	}
	if _, ok := list.Lookup("mallory@example.com"); ok {
		t.Error("an address that is not in the file was allowed")
	}
	if _, ok := list.Lookup(""); ok {
		t.Error("the empty address was allowed")
	}
}

// A typo that silently dropped an operator's own address would be
// discovered only when they could not log in.
//
// @decision(DL-007)
func TestTheSeedRefusesAMalformedLine(t *testing.T) {
	for _, tc := range []struct {
		name, content, wants string
	}{
		{"not an email", "alice\n", "not an e-mail"},
		{"bare at", "@\n", "not an e-mail"},
		{"no local part", "@example.com\n", "not an e-mail"},
		{"no domain", "alice@\n", "not an e-mail"},
		{"two ats", "a@b@c\n", "not an e-mail"},
		{"unknown flag", "alice@example.com owner\n", `not "admin"`},
		{"too many fields", "alice@example.com admin extra\n", "expected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _, err := seed(t, tc.content)
			if err == nil {
				t.Fatal("a malformed line was accepted")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not explain the problem", err)
			}
			// The line number is what makes the message actionable.
			if !strings.Contains(err.Error(), "line 1") {
				t.Errorf("error %q does not name the line", err)
			}
			// A file that failed to parse must leave nothing behind:
			// the import is one transaction, and a half-seeded org
			// would be worse than an unseeded one.
			if list, _ := st.Allowlist().List(context.Background(), "org"); len(list) != 0 {
				t.Errorf("a failed seed left %d entries", len(list))
			}
		})
	}
}

// A server whose allowlist is empty starts and refuses every login,
// which looks like an OAuth problem rather than a mount problem.
//
// @decision(DL-007)
func TestTheSeedRefusesAFileThatAllowsNobody(t *testing.T) {
	for _, content := range []string{"", "# only comments\n", "\n\n  \n"} {
		_, _, err := seed(t, content)
		if !errors.Is(err, ErrEmptyAllowlist) {
			t.Errorf("a seed file of %q gave %v, want ErrEmptyAllowlist", content, err)
		}
	}
}

// The usual cause is a container that did not mount the file.
//
// @decision(DL-007)
func TestTheSeedRefusesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	_, _, err := seedPath(t, path)
	if err == nil {
		t.Fatal("a missing seed file was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}
}

// A bind mount whose source does not exist leaves a *directory* there,
// so this is the shape a mistyped SPOV_ALLOWLIST_FILE actually
// produces. The read error alone ("is a directory") does not say what
// to do about it.
//
// @decision(DL-007)
func TestTheSeedRefusesADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "allowlist")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := seedPath(t, dir)
	if err == nil {
		t.Fatal("a directory was accepted as an allowlist")
	}
	for _, want := range []string{"is a directory", "bind mount", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// An admin line must not be silently undone by a later plain one.
//
// @decision(DL-007)
func TestTheSeedKeepsTheWidestPrivilegeForARepeatedAddress(t *testing.T) {
	for _, content := range []string{
		"alice@example.com admin\nalice@example.com\n",
		"alice@example.com\nALICE@example.com admin\n",
	} {
		_, list, err := seed(t, content)
		if err != nil {
			t.Fatal(err)
		}
		if admin, ok := list.Lookup("alice@example.com"); !ok || !admin {
			t.Errorf("%q gave admin=%v ok=%v", content, admin, ok)
		}
	}
}

// TestTheSeedRunsOnceAndThenTheStoreIsTheTruth pins that after the
// first boot the file cannot undo what the API did.
//
// @decision(DL-007)
func TestTheSeedRunsOnceAndThenTheStoreIsTheTruth(t *testing.T) {
	ctx := context.Background()
	path := writeAllowlist(t, "alice@example.com admin\nbob@example.com\n")
	st := store.NewMemory()

	seeded, err := SeedAllowlist(ctx, st, "org", path, nil)
	if err != nil || !seeded {
		t.Fatalf("first seed: %v, %v", seeded, err)
	}

	// An admin removes bob and adds carol through the API.
	if err := st.Allowlist().Delete(ctx, "org", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "carol@example.com"}); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	seeded, err = SeedAllowlist(ctx, st, "org", path, func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Fatal("the seed ran a second time")
	}
	if _, err := st.Allowlist().Get(ctx, "org", "bob@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the file resurrected an address the API had removed")
	}
	if _, err := st.Allowlist().Get(ctx, "org", "carol@example.com"); err != nil {
		t.Error("the seed dropped an address the API had added")
	}
	// And it says so, rather than leaving an operator to wonder whether
	// editing the file did anything.
	if !hasWarning(warnings, "is ignored") {
		t.Errorf("a skipped seed did not say the file is ignored: %v", warnings)
	}
}

// TestTheSeedReturnsWhenTheOrgHasNoAdmin is the break-glass: an org
// whose last admin went cannot be repaired through the API, which needs
// an admin to authorise the call.
//
// @decision(DL-007)
func TestTheSeedReturnsWhenTheOrgHasNoAdmin(t *testing.T) {
	ctx := context.Background()
	path := writeAllowlist(t, "alice@example.com admin\n")
	st := store.NewMemory()

	// An org with entries but no admin: not empty, so a seed that only
	// checked for emptiness would leave it stranded.
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}
	seeded, err := SeedAllowlist(ctx, st, "org", path, nil)
	if err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v; a non-empty org with no admin must still be seeded", seeded, err)
	}
	got, err := st.Allowlist().Get(ctx, "org", "alice@example.com")
	if err != nil || !got.Admin {
		t.Errorf("alice = %+v, %v", got, err)
	}
	// It upserts rather than replacing, so an entry the file never
	// mentioned survives.
	if _, err := st.Allowlist().Get(ctx, "org", "bob@example.com"); err != nil {
		t.Errorf("the seed dropped an existing entry: %v", err)
	}
}

// A seed file that allows people but names no administrator is not
// refused, but is announced.
//
// @decision(DL-007)
func TestASeedWithNoAdminIsAnnounced(t *testing.T) {
	var warnings []string
	st := store.NewMemory()
	seeded, err := SeedAllowlist(context.Background(), st, "org",
		writeAllowlist(t, "alice@example.com\n"), func(m string) { warnings = append(warnings, m) })
	if err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
	if !hasWarning(warnings, "names no administrator") {
		t.Errorf("warnings = %v", warnings)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// failingStore fails the count the seed decides on.
type failingStore struct {
	store.Store
	err error
}

func (f failingStore) Allowlist() store.AllowlistStore {
	return failingCount{AllowlistStore: f.Store.Allowlist(), err: f.err}
}

func (f failingStore) Tx(ctx context.Context, fn func(store.Store) error) error {
	return f.Store.Tx(ctx, func(tx store.Store) error {
		return fn(failingStore{Store: tx, err: f.err})
	})
}

type failingCount struct {
	store.AllowlistStore
	err error
}

func (f failingCount) CountAdmins(context.Context, string) (int, error) { return 0, f.err }

// A seed that cannot tell whether the org has an administrator must not
// guess: assuming "none" would re-import over a live allowlist, and
// assuming "some" would skip the bootstrap on a fresh deployment.
//
// @decision(DL-007)
func TestTheSeedRefusesToGuessWhenItCannotCount(t *testing.T) {
	boom := errors.New("connection refused")
	st := failingStore{Store: store.NewMemory(), err: boom}
	seeded, err := SeedAllowlist(context.Background(), st, "org",
		writeAllowlist(t, "alice@example.com admin\n"), nil)
	if seeded || !errors.Is(err, boom) {
		t.Fatalf("seeded = %v, err = %v; want the count's error", seeded, err)
	}
}

// TestAMissingSeedFileIsFatalOnlyOnAFirstBoot pins that a missing seed
// file is fatal on a first boot but only a warning on a re-seed.
//
// @decision(DL-007)
func TestAMissingSeedFileIsFatalOnlyOnAFirstBoot(t *testing.T) {
	ctx := context.Background()
	absent := filepath.Join(t.TempDir(), "absent")

	// First boot: nothing in the table, nothing to serve. Fatal.
	if _, err := SeedAllowlist(ctx, store.NewMemory(), "org", absent, nil); err == nil {
		t.Error("a missing file on a first boot was accepted")
	}

	// A running deployment that lost its last admin: degraded, not down.
	st := store.NewMemory()
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	seeded, err := SeedAllowlist(ctx, st, "org", absent, func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatalf("a missing file on a re-seed took the server down: %v", err)
	}
	if seeded {
		t.Error("seeded from a file that does not exist")
	}
	if !hasWarning(warnings, "no administrator") {
		t.Errorf("the degraded state was not announced: %v", warnings)
	}
	// And the entries it could not repair are left alone.
	if _, err := st.Allowlist().Get(ctx, "org", "bob@example.com"); err != nil {
		t.Errorf("an existing entry was lost: %v", err)
	}
}

// The same split applies to a file that parses but allows nobody.
//
// @decision(DL-007)
func TestAnEmptySeedFileIsFatalOnlyOnAFirstBoot(t *testing.T) {
	ctx := context.Background()
	path := writeAllowlist(t, "# everyone went\n")
	st := store.NewMemory()
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SeedAllowlist(ctx, st, "org", path, nil); err != nil {
		t.Errorf("an empty file on a re-seed took the server down: %v", err)
	}
	if _, _, err := seed(t, "# everyone went\n"); !errors.Is(err, ErrEmptyAllowlist) {
		t.Errorf("an empty file on a first boot was accepted: %v", err)
	}
}
