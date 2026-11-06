package cli

import (
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// Snapshots are taken only when enabled, under the proxy, on a server
// whose sessions are in Postgres; bad settings are refused even then.
//
// @decision(DL-084) @decision(DL-086)
func TestSnapshotsNeedTheProxyAndAPersistentStore(t *testing.T) {
	on := config.Defaults().Snapshots
	pg := &postgres.Store{}
	if got, err := snapshotSettings(on, pg, true); err != nil || got == nil || got.Retention != 168*time.Hour || got.MaxPerUser != 20 {
		t.Errorf("hosted under the proxy: %+v, %v", got, err)
	}
	for name, c := range map[string]struct {
		cfg   config.Snapshots
		st    store.Store
		proxy bool
	}{
		"off":       {config.Snapshots{Retention: time.Hour}, pg, true},
		"no proxy":  {on, pg, false},
		"in memory": {on, store.NewMemory(), true},
	} {
		if got, err := snapshotSettings(c.cfg, c.st, c.proxy); err != nil || got != nil {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	bad := on
	bad.Retention = 800 * time.Hour
	if _, err := snapshotSettings(bad, store.NewMemory(), false); err == nil {
		t.Error("a retention past Modal's 30 days was accepted")
	}
}
