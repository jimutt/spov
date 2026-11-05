package sandbox

import (
	"testing"
	"time"
)

func TestBuildTags(t *testing.T) {
	created := time.Date(2026, 8, 28, 22, 30, 0, 0, time.UTC)

	t.Run("full set", func(t *testing.T) {
		tags := BuildTags(TagSet{Name: "morning", User: "alice", Repos: []string{"alice/spov", "acme/api"}, CreatedAt: created})
		want := map[string]string{
			TagSpov:      "1",
			TagName:      "morning",
			TagUser:      "alice",
			TagRepos:     "alice/spov,acme/api",
			TagCreatedAt: "2026-08-28T22:30:00Z",
		}
		for k, v := range want {
			if tags[k] != v {
				t.Errorf("tag %q = %q, want %q", k, tags[k], v)
			}
		}
		if len(tags) != len(want) {
			t.Errorf("tags = %v, want exactly %v", tags, want)
		}
	})

	t.Run("optional tags are omitted rather than empty", func(t *testing.T) {
		tags := BuildTags(TagSet{User: "", CreatedAt: created})
		if _, ok := tags[TagName]; ok {
			t.Error("empty name produced a tag")
		}
		if _, ok := tags[TagUser]; ok {
			t.Error("empty user produced a tag")
		}
		if _, ok := tags[TagRepos]; ok {
			t.Error("empty repos produced a tag")
		}
		if tags[TagSpov] != "1" {
			t.Error("the spov marker must always be present")
		}
	})

	t.Run("timestamps are normalised to UTC", func(t *testing.T) {
		zone := time.FixedZone("CEST", 2*60*60)
		tags := BuildTags(TagSet{CreatedAt: created.In(zone)})
		if tags[TagCreatedAt] != "2026-08-28T22:30:00Z" {
			t.Errorf("created_at = %q, want the UTC instant", tags[TagCreatedAt])
		}
	})
}

func TestOwnedTags(t *testing.T) {
	tags := OwnedTags()
	if tags[TagSpov] != "1" || len(tags) != 1 {
		t.Errorf("OwnedTags() = %v", tags)
	}
	// BuildTags output must match the discovery filter, or `spov ls`
	// silently misses sandboxes spov created.
	created := BuildTags(TagSet{Name: "s", User: "u", Repos: []string{"o/r"}, CreatedAt: time.Now()})
	for k, v := range tags {
		if created[k] != v {
			t.Errorf("BuildTags output does not match OwnedTags on %q", k)
		}
	}
}

func TestIsSandboxID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"sb-ExampleSandboxId000000", true},
		{"sb-", true},
		{"morning", false},
		{"", false},
		{"my-sb-session", false},
	}
	for _, tt := range tests {
		if got := isSandboxID(tt.in); got != tt.want {
			t.Errorf("isSandboxID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestInfoAge(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	t.Run("reports elapsed time", func(t *testing.T) {
		i := Info{CreatedAt: now.Add(-90 * time.Minute)}
		if got := i.Age(now); got != 90*time.Minute {
			t.Errorf("Age = %s, want 90m", got)
		}
	})
	t.Run("zero when the creation time is unknown", func(t *testing.T) {
		if got := (Info{}).Age(now); got != 0 {
			t.Errorf("Age = %s, want 0", got)
		}
	})
}

// JobTags is the filter that finds a job's sandbox; adoption depends
// on it entirely.
//
// @decision(DL-046)
func TestJobTagsMatchWhatBuildTagsWrites(t *testing.T) {
	written := BuildTags(TagSet{
		Name: "job-x", User: "u", Org: "o", Job: "job_123",
		CreatedAt: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
	})
	for k, v := range JobTags("job_123") {
		if written[k] != v {
			t.Errorf("filter wants %s=%q but BuildTags wrote %q;"+
				" the query would match nothing and every retry would launch again", k, v, written[k])
		}
	}
	// And it must not match a sandbox belonging to a different job.
	other := BuildTags(TagSet{Name: "job-y", Job: "job_456", CreatedAt: time.Now()})
	filter := JobTags("job_123")
	if other[TagJob] == filter[TagJob] {
		t.Error("the filter does not distinguish one job's sandbox from another's")
	}
}

// An interactive session carries no job tag, so a job's filter can
// never adopt one.
//
// @decision(DL-046)
func TestAnInteractiveSessionCarriesNoJobTag(t *testing.T) {
	tags := BuildTags(TagSet{Name: "morning", CreatedAt: time.Now()})
	if _, ok := tags[TagJob]; ok {
		t.Error("an interactive session was tagged with a job")
	}
}

// ReposFromTags reads back exactly what BuildTags wrote.
func TestReposRoundTripThroughTags(t *testing.T) {
	repos := []string{"alice/spov", "acme/api"}
	got := ReposFromTags(BuildTags(TagSet{Repos: repos, CreatedAt: time.Now()}))
	if len(got) != len(repos) {
		t.Fatalf("read back %v, want %v", got, repos)
	}
	for i := range repos {
		if got[i] != repos[i] {
			t.Errorf("repo %d = %q, want %q", i, got[i], repos[i])
		}
	}
	if got := ReposFromTags(map[string]string{}); got != nil {
		t.Errorf("no repo tag should read back as nil, got %v", got)
	}
}
