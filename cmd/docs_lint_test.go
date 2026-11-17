package cmd

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The docs lint holds every command the Markdown shows to the command
// tree. A fenced shell block is read line by line; each `heo ...`
// invocation must name a runnable command and pass only flags it (or a
// global) declares. A concrete invocation is held to everything
// Execute's parser checks (argument count, required flags, flag
// groups). A synopsis (`[--limit N]`, `a | b`, `...`) is held to the
// verb and the flags it names, since it is not meant to run as written.

// docRepoRoot is the repository root, from package cmd's directory.
const docRepoRoot = ".."

// docSkipDirs are never searched for Markdown.
var docSkipDirs = map[string]bool{
	".git": true, ".tlc": true, "bin": true, "tests": true, "node_modules": true,
	"e2e": true, ".github": true, ".kit": true, "dist": true, "conformance": true,
}

// docSkipFiles are local, untracked or release-owned files.
var docSkipFiles = map[string]bool{"AGENTS.md": true, "CLAUDE.md": true, "CHANGELOG.md": true}

// markdownFiles lists the repository's Markdown, relative to the root.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(docRepoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != docRepoRoot && docSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") && !docSkipFiles[d.Name()] {
			rel, _ := filepath.Rel(docRepoRoot, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// shellFences are the info strings of blocks whose lines are commands.
var shellFences = map[string]bool{"sh": true, "bash": true, "shell": true, "zsh": true, "console": true}

// docCommand is one `heo ...` invocation found in a Markdown file.
type docCommand struct {
	file     string
	line     int
	text     string
	synopsis bool
	// continuation is a line that only adds flags to the previous command.
	continuation bool
}

var (
	envAssign   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s+`)
	substituted = regexp.MustCompile(`\$\(\s*(heo\s[^)]*)\)`)
	synopsisRe  = regexp.MustCompile(`\[|\]|\.\.\.`)
	// inlineHeo matches a command in a code span in prose or a table
	// cell; those are held to the verb and flags only.
	inlineHeo = regexp.MustCompile("`(heo [^`]+)`")
)

// extractCommands reads the heo invocations out of the fenced shell
// blocks of one file.
func extractCommands(t *testing.T, file string) []docCommand {
	t.Helper()
	f, err := os.Open(filepath.Join(docRepoRoot, file)) //nolint:gosec // G304: a repository Markdown file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var out []docCommand
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n, inFence, lintable := 0, false, false
	var joined strings.Builder
	joinedAt := 0
	prevWasHeo := false
	for sc.Scan() {
		n++
		raw := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			if inFence {
				inFence, lintable, prevWasHeo = false, false, false
				joined.Reset()
			} else {
				info := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "```"))
				if i := strings.IndexAny(info, " \t"); i > 0 {
					info = info[:i]
				}
				inFence, lintable = true, shellFences[info]
			}
			continue
		}
		if !inFence {
			for _, m := range inlineHeo.FindAllStringSubmatch(raw, -1) {
				if w := strings.Fields(m[1]); len(w) > 1 && !strings.HasPrefix(w[1], "<") {
					out = append(out, docCommand{file: file, line: n, text: m[1], synopsis: true})
				}
			}
			continue
		}
		if !lintable {
			continue
		}
		line := strings.TrimSpace(raw)
		if joined.Len() > 0 { // a backslash continued the previous line
			line = joined.String() + " " + line
			joined.Reset()
			n = joinedAt
		}
		if strings.HasSuffix(line, `\`) {
			joined.WriteString(strings.TrimSpace(strings.TrimSuffix(line, `\`)))
			joinedAt = n
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			prevWasHeo = false
			continue
		}
		// An indented line that starts with a flag or an option group
		// continues the synopsis above it.
		if prevWasHeo && (raw[0] == ' ' || raw[0] == '\t') && (strings.HasPrefix(line, "[") || strings.HasPrefix(line, "-")) {
			out = append(out, docCommand{file: file, line: n, text: stripComment(line), synopsis: true, continuation: true})
			continue
		}
		cmds := heoCommandsIn(line)
		prevWasHeo = len(cmds) > 0
		for _, c := range cmds {
			out = append(out, docCommand{file: file, line: n, text: c, synopsis: isSynopsis(c)})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// stripComment drops a trailing ` # comment` that is outside quotes.
func stripComment(s string) string {
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && i > 0 && s[i-1] == ' ':
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

// heoCommandsIn finds the invocations on one shell line: the command
// the line starts with (after environment assignments, `$ ` or `(`) and
// each `$(heo ...)` substitution.
func heoCommandsIn(line string) []string {
	var out []string
	for _, m := range substituted.FindAllStringSubmatch(line, -1) {
		out = append(out, stripComment(m[1]))
	}
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$ "))
	s = strings.TrimPrefix(s, "(")
	for envAssign.MatchString(s) {
		s = envAssign.ReplaceAllString(s, "")
	}
	if s == "heo" || strings.HasPrefix(s, "heo ") {
		s = stripComment(s)
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "&"))
		out = append([]string{s}, out...)
	}
	return out
}

// unquoted blanks the quoted parts of s, keeping its length.
func unquoted(s string) string {
	var quote rune
	b := []rune(s)
	for i, r := range b {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			b[i] = '_'
		case r == '\'' || r == '"':
			quote = r
			b[i] = '_'
		}
	}
	return string(b)
}

// generatedDoc reports whether a file is rendered by `make docs-gen`.
func generatedDoc(t *testing.T, file string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docRepoRoot, file)) //nolint:gosec // G304: a repository Markdown file
	if err != nil {
		t.Fatal(err)
	}
	return strings.HasPrefix(string(b), "<!-- Generated by `make docs-gen`")
}

// rootFlagOnly reports whether a documented line is `heo` with only a
// flag of the root command itself, such as `heo --version`.
func rootFlagOnly(c string) bool {
	w := strings.Fields(c)
	if len(w) != 2 || w[0] != "heo" {
		return false
	}
	switch w[1] {
	case "--version", "-v", "--help", "-h", "--help-all", "--help-management":
		return true
	}
	return false
}

// isSynopsis reports whether a command line is a usage synopsis rather
// than something to paste: option brackets, an ellipsis, or an
// alternation of verbs.
func isSynopsis(c string) bool {
	u := unquoted(c)
	return synopsisRe.MatchString(u) || strings.Contains(u, " | ")
}

// docFlag finds the flag a documented word names on c: long, short, or
// `--name=value`.
func docFlag(c *cobra.Command, word string) bool {
	name := strings.TrimLeft(word, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	if name == "" || name == "-" {
		return true // `-` alone, or `--`
	}
	switch name {
	case "help", "h":
		return true // cobra adds the help flag when a command executes
	case "version", "v", "help-all", "help-management":
		return c == c.Root() // the root's own, added the same way
	}
	if len(name) == 1 && !strings.HasPrefix(word, "--") {
		return c.Flags().ShorthandLookup(name) != nil || c.InheritedFlags().ShorthandLookup(name) != nil ||
			c.Root().PersistentFlags().ShorthandLookup(name) != nil
	}
	return c.Flag(name) != nil || c.InheritedFlags().Lookup(name) != nil ||
		c.Root().PersistentFlags().Lookup(name) != nil
}

// checkSynopsis holds a synopsis to the verb it names and the flags it
// shows. Returns a problem or "".
func checkSynopsis(t *testing.T, a *app, c docCommand, prev *cobra.Command) (*cobra.Command, string) {
	t.Helper()
	clean := strings.NewReplacer("[", " ", "]", " ").Replace(c.text)
	var segs []string
	if strings.Contains(unquoted(clean), " | ") {
		// Split on top-level pipes only.
		u := unquoted(clean)
		last := 0
		for {
			i := strings.Index(u[last:], " | ")
			if i < 0 {
				break
			}
			segs = append(segs, clean[last:last+i])
			last += i + 3
		}
		segs = append(segs, clean[last:])
	} else {
		segs = []string{clean}
	}

	var leaf *cobra.Command
	for i, seg := range segs {
		words := shellWords(t, seg)
		words = slices.DeleteFunc(words, func(w string) bool { return w == "..." || w == "" })
		switch {
		case c.continuation && i == 0:
			leaf = prev
		case i == 0:
			if len(words) == 0 || words[0] != "heo" {
				return nil, "does not start with heo"
			}
			args := words[1:]
			found, rest, err := a.r.Cmd.Find(args)
			if err != nil {
				return nil, err.Error()
			}
			if found == a.r.Cmd && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
				return nil, "unknown command " + args[0]
			}
			if found.HasSubCommands() {
				// A word that is neither a flag nor a placeholder, where a
				// subcommand belongs, names a verb that does not exist.
				for _, w := range rest {
					if !strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "<") {
						return nil, "unknown command " + found.CommandPath() + " " + w
					}
					break
				}
			}
			leaf = found
			words = rest
		default:
			// After a pipe: another verb of the same group, more flags
			// of this one, or a shell consumer (ignored).
			if len(words) == 0 {
				continue
			}
			w := words[0]
			if strings.HasPrefix(w, "-") {
				break
			}
			if leaf == nil || leaf.Parent() == nil {
				continue
			}
			var sib *cobra.Command
			for _, s := range leaf.Parent().Commands() {
				if s.Name() == w {
					sib = s
				}
			}
			if sib == nil {
				continue // a pipeline into a shell tool
			}
			leaf, words = sib, words[1:]
		}
		if leaf == nil {
			continue
		}
		for _, w := range words {
			if strings.HasPrefix(w, "-") && !docFlag(leaf, w) {
				return nil, "flag " + w + " is not declared by " + leaf.CommandPath() + " or a global"
			}
		}
	}
	return leaf, ""
}

// TestDocsCommandsAreReal: every `heo ...` invocation in the Markdown
// is a command the tree has. A documented verb, flag or argument count
// that does not exist fails here, at the line that shows it.
func TestDocsCommandsAreReal(t *testing.T) {
	upgradeSandbox(t)
	a := newApp()
	if err := a.r.Prepare(); err != nil {
		t.Fatal(err)
	}
	var total, concrete int
	perFile := map[string]int{}
	seen := map[string]bool{}
	for _, file := range markdownFiles(t) {
		if generatedDoc(t, file) {
			// Rendered from the examples TestGuidanceExamplesParseAgainstTheTree
			// already parses; parsing 400 lines again is a 10 s cost for nothing.
			continue
		}
		var prev *cobra.Command
		for _, c := range extractCommands(t, file) {
			total++
			perFile[file]++
			where := file + ":" + strconv.Itoa(c.line)
			if c.synopsis {
				leaf, problem := checkSynopsis(t, a, c, prev)
				if problem != "" {
					t.Errorf("%s: %q: %s", where, c.text, problem)
				}
				if leaf != nil {
					prev = leaf
				}
				continue
			}
			if seen[c.text] || rootFlagOnly(c.text) {
				continue
			}
			seen[c.text] = true
			concrete++
			cmd, _, err := resolveInvocation(t, c.text)
			if err != nil {
				t.Errorf("%s: %q: %v", where, c.text, err)
			}
			prev = cmd
		}
	}
	t.Logf("docs lint: %d invocations (%d concrete) in %d files", total, concrete, len(perFile))
	// The lint must not pass by finding nothing.
	for _, f := range []string{
		"README.md", "SKILL.md", "docs/manual/getting-started.md", "docs/manual/install.md",
		"docs/manual/commands.md", "docs/manual/conversations.md", "docs/manual/surfaces.md",
	} {
		if perFile[f] == 0 {
			t.Errorf("docs lint found no heo invocation in %s", f)
		}
	}
}

// internalReferences are things a repository file must never carry:
// task or track IDs, local paths, internal stores, tool or agent names.
var internalReferences = []*regexp.Regexp{
	regexp.MustCompile(`\bT-[0-9]{3,}\b`),
	regexp.MustCompile(`/Users/[A-Za-z0-9._-]+`),
	regexp.MustCompile(`/home/[a-z][a-z0-9._-]*/\.w/`),
	regexp.MustCompile(`\.docs/`),
	regexp.MustCompile(`APS_DATA_PATH`),
	regexp.MustCompile(`~/\.(rlz|ops|agents)\b`),
	regexp.MustCompile(`(?i)\btlc\b`),
	regexp.MustCompile(`(?i)ideacrafterslabs`),
	regexp.MustCompile(`(?i)\b(claude|anthropic|codex|copilot)\b`),
	regexp.MustCompile(`(?i)co-authored-by`),
}

// TestDocsCarryNoInternalReferences: the Markdown describes behaviour;
// decisions, plans and tasks live outside the repository and are never
// cited by ID or path.
func TestDocsCarryNoInternalReferences(t *testing.T) {
	for _, file := range markdownFiles(t) {
		b, err := os.ReadFile(filepath.Join(docRepoRoot, file)) //nolint:gosec // G304: a repository Markdown file
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, re := range internalReferences {
				if m := re.FindString(line); m != "" {
					t.Errorf("%s:%d: internal reference %q", file, i+1, m)
				}
			}
		}
	}
}

// TestDocsLineBudget: prose lines stay within 100 columns and table
// rows within 256, the repository's Markdown limits, in the files the
// docs set owns. Code fences, links-only lines and the generated files
// (held by their own test) are exempt.
func TestDocsLineBudget(t *testing.T) {
	for _, file := range []string{
		"README.md", "SKILL.md", "docs/ARCHITECTURE.md", "docs/RELEASING.md",
		"docs/manual/getting-started.md", "docs/manual/install.md", "docs/manual/troubleshooting.md",
		"docs/manual/llm-helpers.md",
	} {
		b, err := os.ReadFile(filepath.Join(docRepoRoot, file)) //nolint:gosec // G304: a repository Markdown file
		if err != nil {
			t.Fatal(err)
		}
		inFence := false
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "```") {
				inFence = !inFence
				continue
			}
			limit := 100
			switch {
			case strings.HasPrefix(line, "|"):
				limit = 256
			case inFence, strings.HasPrefix(line, "description:"), strings.HasPrefix(line, "[!["):
				continue
			}
			if n := len([]rune(line)); n > limit {
				t.Errorf("%s:%d: %d columns, limit %d", file, i+1, n, limit)
			}
		}
	}
}
