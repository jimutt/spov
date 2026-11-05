package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/sahilm/fuzzy"
)

func TestProbeShiftUTF8(t *testing.T) {
	cf := "codefloe"
	gh := "gh"
	repos := []RepoChoice{
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "kläderä"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "x"}},
	}
	p := newRepoPicker(repos, true, time.Now())
	for _, r := range p.rows {
		fmt.Printf("haystack=%q bytes=%d slugLen=%d slugPad=%d render=%q renderWidth=%d\n",
			r.haystack, len(r.haystack), r.slugLen, r.slugPad, r.render(), len(r.render()))
	}
	for _, q := range []string{"ä", "kläder", "codefloe", "äc"} {
		hs := []string{p.rows[0].haystack, p.rows[1].haystack}
		ms := fuzzy.Find(q, hs)
		for _, m := range ms {
			row := p.rows[m.Index]
			sh := row.shift(m.MatchedIndexes)
			out := stripStyle(highlight(row.render(), sh, p.styles.match))
			fmt.Printf("q=%q matched=%v shifted=%v out=%q corrupt=%v\n", q, m.MatchedIndexes, sh, out, out != row.render())
		}
	}
	// pathological: query char lands exactly on the separator space
	fmt.Println("--- separator space match ---")
	hs := []string{p.rows[0].haystack}
	ms := fuzzy.Find("r c", hs)
	for _, m := range ms {
		row := p.rows[0]
		sh := row.shift(m.MatchedIndexes)
		fmt.Printf("matched=%v shifted=%v\n", m.MatchedIndexes, sh)
		res := highlight(row.render(), sh, p.styles.match)
		fmt.Printf("highlighted(stripped)=%q equalsRender=%v\n", stripStyle(res), stripStyle(res) == row.render())
		fmt.Printf("visible-with-escapes: %s\n", strings.ReplaceAll(res, "\x1b", "ESC"))
	}
}
