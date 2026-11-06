package cli

import (
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
)

// Snapshots are taken only when enabled, under the proxy, on a server
// that keeps its sessions; bad settings are refused even then.
//
// @decision(DL-084) @decision(DL-086) @decision(DL-091)
func TestSnapshotsNeedTheProxyAndAPersistentStore(t *testing.T) {
	on := config.Defaults().Snapshots
	if got, err := snapshotSettings(on, true, true); err != nil || got == nil || got.Retention != 168*time.Hour || got.MaxPerUser != 20 {
		t.Errorf("a persistent server under the proxy: %+v, %v", got, err)
	}
	for name, c := range map[string]struct {
		cfg        config.Snapshots
		persistent bool
		proxy      bool
	}{
		"off":       {config.Snapshots{Retention: time.Hour}, true, true},
		"no proxy":  {on, true, false},
		"in memory": {on, false, true},
	} {
		if got, err := snapshotSettings(c.cfg, c.persistent, c.proxy); err != nil || got != nil {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	bad := on
	bad.Retention = 800 * time.Hour
	if _, err := snapshotSettings(bad, false, false); err == nil {
		t.Error("a retention past Modal's 30 days was accepted")
	}
}
