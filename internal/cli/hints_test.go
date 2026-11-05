package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// proseWords follow "spov " in sentences about spov rather than in a
// command ("spov cannot reach…"). A new sentence of that shape fails
// TestEveryNamedCommandExists until its word is added here. Inside
// backticks or a <code> element the word is checked all the same.
var proseWords = map[string]bool{
	"can": true, "cannot": true, "configuration": true, "continues": true, "control": true,
	"could": true, "did": true, "judges": true, "keeps": true, "knows": true,
	"launches": true, "manage": true, "mark": true, "marker": true, "may": true,
	"never": true, "prepared": true, "prints": true, "proposed": true, "reads": true,
	"reserves": true, "said": true, "sandbox": true, "sends": true, "server": true,
	"session": true, "sessions": true, "sets": true, "stops": true, "stores": true,
	"talking": true, "understands": true, "uses": true, "will": true, "writes": true,
	"wrote": true,
}

var namedCommand = regexp.MustCompile(`\bspov ([a-z][a-z-]*)`)

// TestEveryNamedCommandExists: every `spov <command>` written in a Go
// string literal under internal/ and cmd/, or anywhere in the web
// source, resolves to a command in the tree, and every --flag written
// after it is one that command takes. Tests are out of scope.
//
// @decision(DL-023)
func TestEveryNamedCommandExists(t *testing.T) {
	root := NewRoot(Env{})
	repo := filepath.Join("..", "..")
	var checked int
	check := func(where, text string) {
		for _, loc := range namedCommand.FindAllStringSubmatchIndex(text, -1) {
			if proseWords[text[loc[2]:loc[3]]] && !quotedAt(text, loc[0]) {
				continue
			}
			checked++
			if problem := resolveNamed(root, commandSpan(text[loc[0]:])); problem != "" {
				t.Errorf("%s: %q %s", where, commandSpan(text[loc[0]:]), problem)
			}
		}
	}
	for _, dir := range []string{"internal", "cmd"} {
		walk(t, filepath.Join(repo, dir), func(path string) {
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if s, err := strconv.Unquote(lit.Value); err == nil {
					check(path, s)
				}
				return true
			})
		})
	}
	walk(t, filepath.Join(repo, "web", "src"), func(path string) {
		ext := filepath.Ext(path)
		if (ext != ".ts" && ext != ".svelte" && ext != ".astro") || strings.Contains(path, ".test.") {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(path, string(b))
	})
	if checked < 30 {
		t.Fatalf("only %d named commands found; the scan is not reaching the sources", checked)
	}
}

func walk(t *testing.T, dir string, fn func(string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "testdata" || d.Name() == "dist") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			fn(path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// commandSpan cuts a named command out of its sentence: it ends at a
// closing backtick, a closing tag, a quote, a comment, the end of the
// line, or a word that ends a clause ("spov config, or pass …"). A
// placeholder such as <url> stays in it.
func commandSpan(s string) string {
	if i := strings.IndexAny(s, "`\"'#\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "</"); i >= 0 {
		s = s[:i]
	}
	fields := strings.Fields(s)
	for i, f := range fields {
		if strings.ContainsAny(f[len(f)-1:], ",.;:)") {
			fields[i] = strings.TrimRight(f, ",.;:)")
			return strings.Join(fields[:i+1], " ")
		}
	}
	return strings.Join(fields, " ")
}

// quotedAt reports whether the text at i opens a backtick span or a
// <code> element.
func quotedAt(text string, i int) bool {
	before := text[:i]
	if strings.HasSuffix(before, "`") {
		return true
	}
	if !strings.HasSuffix(before, ">") {
		return false
	}
	return strings.HasPrefix(before[strings.LastIndex(before, "<"):], "<code")
}

var bareWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// resolveNamed walks span ("spov jobs cancel %s --x") down the tree and
// says what does not exist, or "" when it all does. A bare word after a
// group is taken for a subcommand it lacks, since no group takes one as
// an argument.
func resolveNamed(root *cobra.Command, span string) string {
	fields := strings.Fields(span)[1:]
	cmd := root
	depth := 0
	for _, w := range fields {
		next := child(cmd, w)
		if next == nil {
			break
		}
		cmd = next
		depth++
	}
	if cmd == root {
		return "names no command"
	}
	if depth < len(fields) && cmd.HasSubCommands() && bareWord.MatchString(fields[depth]) {
		return "names no subcommand " + fields[depth] + " of `" + cmd.CommandPath() + "`"
	}
	for _, w := range fields {
		if !strings.HasPrefix(w, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(w, "--"), "=")
		if lookupFlag(cmd, name) == nil {
			return "uses --" + name + ", which `spov " + strings.TrimPrefix(cmd.CommandPath(), "spov ") + "` does not take"
		}
	}
	return ""
}

func child(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return c
		}
	}
	return nil
}

func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	cmd.InitDefaultHelpFlag()
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	return cmd.InheritedFlags().Lookup(name)
}

// TestTheHintGuardCatchesAStaleCommand: the guard's own parsing, on the
// shapes it meets in the sources.
//
// @decision(DL-023)
func TestTheHintGuardCatchesAStaleCommand(t *testing.T) {
	root := NewRoot(Env{})
	tests := []struct {
		text string
		ok   bool
	}{
		{"run `spov ls` to see what is running", true},
		{"  stop:    spov nosuch %s\n", false},
		{"cancel it with `spov jobs cancel %s`: %w", true},
		{"`spov jobs nosuch` names a subcommand the group lacks", false},
		{"`spov jobs show %s`", true},
		{"`spov serve image prun`", false},
		{"`spov jobs list --help`", true},
		{"`spov login --server <url>` points this CLI at one", true},
		{"`spov login --nosuch <url>`", false},
		{`<code class="font-mono">spov jobs create</code> send`, true},
		{"`spov attach %s --web`", true},
		{"`spov attach %s --nosuch`", false},
		{"Run: spov login --server %s", true},
		{"add git.name to your spov config, or pass --git-name", true},
		{"spov serve: applied 3 migration(s)", true},
		{"`spov nosuch --web`", false},
	}
	for _, tt := range []struct {
		text string
		at   string
		want bool
	}{
		{"`spov server`", "spov", true},
		{`<code class="x">spov session</code>`, "spov", true},
		{"<p>spov could not log you in</p>", "spov", false},
		{"so spov cannot", "spov", false},
	} {
		if got := quotedAt(tt.text, strings.Index(tt.text, tt.at)); got != tt.want {
			t.Errorf("quotedAt(%q) = %v", tt.text, got)
		}
	}
	for _, tt := range tests {
		span := commandSpan(tt.text[strings.Index(tt.text, "spov "):])
		problem := resolveNamed(root, span)
		if (problem == "") != tt.ok {
			t.Errorf("%q: span %q, problem %q", tt.text, span, problem)
		}
	}
}
