package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// No SQL is built from request text. Every query is sqlc's or a string
// literal, except the sessions listing, which assembles its WHERE clause
// from fixed fragments and binds every value as a parameter.
//
// @decision(DL-003) @decision(DL-006)

// TestSessionFilterValuesAreBoundNeverWritten feeds the listing's filter
// text that would change the query if it were written into it, and
// checks the SQL holds only fixed words and placeholders.
func TestSessionFilterValuesAreBoundNeverWritten(t *testing.T) {
	hostile := "x' or '1'='1'; drop table sessions; --"
	f := store.SessionFilter{
		OwnerID: hostile, Name: hostile, Before: hostile, Limit: 5,
		States: []store.SessionState{store.SessionState(hostile)},
	}
	var sb strings.Builder
	args := sessionWhere(&sb, "select *", hostile, f)
	sql := sb.String()
	if strings.Contains(sql, "drop") || strings.Contains(sql, "'") || strings.Contains(sql, "--") {
		t.Fatalf("the filter's text reached the SQL: %s", sql)
	}
	shape := regexp.MustCompile(`^select \* from sessions where org_id = \$1( and (owner_id = \$\d+|name = \$\d+|state = any\(\$\d+\)))*$`)
	if !shape.MatchString(sql) {
		t.Errorf("the SQL is not made of the fixed fragments and placeholders: %s", sql)
	}
	// The organisation is always the first condition, so no filter can
	// replace it.
	if !strings.HasPrefix(sql, "select * from sessions where org_id = $1") || args[0] != hostile {
		t.Errorf("the org is not the first bound condition: %s %v", sql, args)
	}
	for _, want := range []string{hostile} {
		var bound int
		for _, a := range args {
			switch v := a.(type) {
			case string:
				if v == want {
					bound++
				}
			case []string:
				if len(v) == 1 && v[0] == want {
					bound++
				}
			}
		}
		if bound != 4 {
			t.Errorf("%d of the 4 values (org, owner, name, state) were bound as arguments, want all: %v", bound, args)
		}
	}
}

// TestOnlyTheSessionListingBuildsSQLAtRunTime reads the package's source
// for every call that runs SQL and requires the text to be a literal,
// with the two exceptions that take their text from sessionWhere.
func TestOnlyTheSessionListingBuildsSQLAtRunTime(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var built []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Query" && sel.Sel.Name != "QueryRow" && sel.Sel.Name != "Exec") {
				return true
			}
			if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				return true
			}
			built = append(built, name)
			return true
		})
	}
	// List and Count, both in sessions.go.
	if len(built) != 2 || built[0] != "sessions.go" || built[1] != "sessions.go" {
		t.Errorf("SQL text that is not a literal is run from %v, want only sessions.go's List and Count: "+
			"a query built any other way needs the same proof as sessionWhere that no request text reaches it", built)
	}
}
