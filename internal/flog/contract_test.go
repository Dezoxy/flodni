package flog

import (
	"bytes"
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
)

// stableLines are the lines operators grep. Each entry lists fragments that
// must appear, in order, in one message literal somewhere in the source, and
// must survive the default text handler. A reworded line fails here, and so
// does a move to a handler that changes the message.
//
// Lines the daemon prints through the standard logger (the GoStorm fork's
// TLogln) are checked for presence only: they do not pass through flog.
var stableLines = []struct {
	name  string
	frags []string
	std   bool
}{
	{"disk warmup size", []string{"[DiskWarmup] Initial size: ", "(dropped ", " stale cache files"}, false},
	{"disk warmup active", []string{"[DiskWarmup] Active"}, false},
	{"disk warmup corrupt cache", []string{"CORRUPT CACHE"}, false},
	{"state db migration already completed", []string{"[StateDB] Migration already completed"}, false},
	{"state db migration complete", []string{"[StateDB] Migration complete:"}, false},
	{"state db active", []string{"[StateDB] Active: "}, false},
	{"state db open failed", []string{"Failed to open StateDB"}, false},
	{"inode map loaded", []string{"InodeMap: Loaded ", " files"}, false},
	{"fuse mounted", []string{"FUSE mounted at "}, false},
	{"fuse bdi", []string{"[FUSE] mounted, bdi ", ", read_ahead_kb "}, false},
	{"library torrents without engine record", []string{"Library torrents with no stored engine record with metainfo: ", " of "}, false},
	{"config parse failure", []string{"Failed to parse"}, false},
	{"path differs", []string{"physical_source_path", "differs"}, false},
	{"startup gc", []string{"Startup GC: Pruned"}, false},
	{"prune skipped", []string{"prune skipped"}, false},
	{"session line prefix", []string{"[Session] "}, false},
	{"warm line prefix", []string{"[Warm] "}, false},
	{"startup report", []string{"[Startup] report"}, false},
	{"dead swarm", []string{"[DeadSwarm] "}, false},
	{"webhook status", []string{"[Webhook] "}, false},
	{"settings migrated", []string{"Successfully migrated "}, true},
	{"settings corrupt", []string{"Settings file was corrupt"}, true},
	{"torrent migrated", []string{"Migrate torrent"}, true},
}

var verb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

func TestStableLinesAreInTheSourceAndSurviveTheTextHandler(t *testing.T) {
	lits := sourceLiterals(t)
	for _, s := range stableLines {
		var hit string
		for _, l := range lits {
			if inOrder(l, s.frags) {
				hit = l
				break
			}
		}
		if hit == "" {
			t.Errorf("%s: no message literal contains %q in order", s.name, s.frags)
			continue
		}
		if s.std {
			continue
		}
		var buf bytes.Buffer
		New(&buf, "GoProxy", true).Info(verb.ReplaceAllString(hit, "N"))
		if !inOrder(buf.String(), s.frags) {
			t.Errorf("%s: default text output %q lost %q", s.name, buf.String(), s.frags)
		}
	}
}

func TestStableLoggerNamesAndShellLine(t *testing.T) {
	// [WarmupEvict] is the logger name of the eviction line: "[WarmupEvict] <time> {json}".
	found := false
	for _, l := range sourceLiterals(t) {
		if l == "WarmupEvict" {
			found = true
		}
	}
	if !found {
		t.Error(`no logger named "WarmupEvict"`)
	}
	var buf bytes.Buffer
	New(&buf, "WarmupEvict", true).Info(`{"hash":"x"}`)
	if !strings.HasPrefix(buf.String(), "[WarmupEvict] ") || !strings.HasSuffix(buf.String(), " {\"hash\":\"x\"}\n") {
		t.Errorf("eviction line shape: %q", buf.String())
	}
	// the container entrypoint prints this one from the shell
	sh, err := os.ReadFile(filepath.Join(repoRoot(t), "docker", "docker-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sh), "is NOT on a mounted volume") {
		t.Error("docker-entrypoint.sh lost the 'is NOT on a mounted volume' line")
	}
}

func inOrder(s string, frags []string) bool {
	for _, f := range frags {
		i := strings.Index(s, f)
		if i < 0 {
			return false
		}
		s = s[i+len(f):]
	}
	return true
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// sourceLiterals returns every string literal (a concatenation counts as one)
// in the non-test Go source outside the vendored engine.
func sourceLiterals(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "anacrolix-torrent" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if e, ok := n.(ast.Expr); ok {
					if s, ok := concat(e); ok {
						out = append(out, s)
						if _, isBin := e.(*ast.BinaryExpr); isBin {
							return false // the parts are inside the whole
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func concat(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok1 := concat(e.X)
		r, ok2 := concat(e.Y)
		return l + r, ok1 && ok2
	}
	return "", false
}
