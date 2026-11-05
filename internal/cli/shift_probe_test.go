package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/sahilm/fuzzy"
)

func TestProbeShift(t *testing.T) {
	cf := "codefloe"
	gh := "gh"
	repos := []RepoChoice{
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "spov"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "spov-docs"}},
	}
	p := newRepoPicker(repos, true, time.Now())
	for _, r := range p.rows {
		fmt.Printf("haystack=%q slugLen=%d slugPad=%d platformPad=%d render=%q\n", r.haystack, r.slugLen, r.slugPad, r.platformPad, r.render())
	}
	// probe offsets at the boundary
	for _, q := range []string{"codefloe", "gh", "spov", "t c", "s"} {
		hs := []string{p.rows[0].haystack, p.rows[1].haystack}
		ms := fuzzy.Find(q, hs)
		for _, m := range ms {
			row := p.rows[m.Index]
			fmt.Printf("q=%q idx=%d matched=%v shifted=%v render=%q\n", q, m.Index, m.MatchedIndexes, row.shift(m.MatchedIndexes), row.render())
			// verify runes at shifted positions equal runes at original in haystack
			for k, oi := range m.MatchedIndexes {
				si := row.shift(m.MatchedIndexes)[k]
				rr := []byte(row.render())
				hh := []byte(row.haystack)
				if si >= len(rr) {
					fmt.Printf("   OUT OF RANGE shifted=%d len(render)=%d\n", si, len(rr))
					continue
				}
				if rr[si] != hh[oi] {
					fmt.Printf("   MISMATCH orig[%d]=%q shifted[%d]=%q\n", oi, hh[oi], si, rr[si])
				}
			}
		}
	}
}
