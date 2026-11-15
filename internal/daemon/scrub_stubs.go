package daemon

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"flodni/internal/library"
)

// scrubEuid is the effective uid; a test replaces it.
var scrubEuid = os.Geteuid

const scrubUsage = `usage: flodni scrub-stubs [--dry-run] [--allow-root] <source_path>
  source_path is the directory the daemon is given as its source (it holds movies/ and tv/);
  flags go before it. Run it as the daemon's user.`

// scrubStubsMain is `flodni scrub-stubs`: the owner-run pass that rewrites the magnet
// of every existing stub to the bare info hash, so a tracker passkey an older version
// wrote is gone from disk. It starts no daemon and touches no mount; it walks
// <source_path>/movies and <source_path>/tv, the stub tree the library uses. It prints
// one summary line and never a magnet or a URL. Exit 0 when it scanned stubs without
// error, 1 when a stub could not be handled, 2 for a bad invocation, a source with
// neither movies/ nor tv/, a run as root without --allow-root, or nothing scanned.
func scrubStubsMain(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("scrub-stubs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "count the stubs that would be rewritten, change nothing")
	allowRoot := fs.Bool("allow-root", false, "run as root (files keep their owner, but root follows what the tree's owner can plant)")
	usage := func(why string) int {
		_, _ = fmt.Fprintf(errOut, "scrub-stubs: %s\n%s\n", why, scrubUsage)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return usage(err.Error())
	}
	// flag stops at the first positional, so a flag after it would be ignored and a
	// dry run would write: exactly one argument, and it is the path.
	if f := misplacedFlag(fs.Args()); f != "" {
		return usage(fmt.Sprintf("usage error, nothing was changed: %s must come before the source path", f))
	}
	if fs.NArg() != 1 {
		return usage("usage error, nothing was changed: exactly one source path is required, after the flags")
	}
	source := fs.Arg(0)
	if len(source) > 0 && source[0] == '-' {
		return usage("flags go before the source path")
	}
	if scrubEuid() == 0 && !*allowRoot {
		return usage("refusing to run as root: a tree writable by another user could redirect a root run; run it as the daemon's user, or pass --allow-root")
	}
	roots := []string{filepath.Join(source, "movies"), filepath.Join(source, "tv")}
	dirs := 0
	for _, r := range roots {
		if fi, err := os.Lstat(r); err == nil && fi.IsDir() {
			dirs++
		}
	}
	if dirs == 0 {
		return usage("neither movies/ nor tv/ is a directory under the source path (is this the source, not the mount?)")
	}

	res := library.ScrubStubs(roots, *dryRun)
	for _, e := range res.Errors {
		_, _ = fmt.Fprintf(errOut, "scrub-stubs: %s: %s\n", e.Path, e.Reason)
	}
	mode := ""
	if *dryRun {
		mode = " (dry run: nothing written, rewritten counts the stubs a real run would rewrite)"
	}
	_, _ = fmt.Fprintf(out, "scrub-stubs: scanned=%d rewritten=%d skipped=%d errors=%d%s\n",
		res.Scanned, res.Rewritten, res.Skipped, len(res.Errors), mode)
	if len(res.Errors) > 0 {
		return 1
	}
	if res.Scanned == 0 {
		_, _ = fmt.Fprintln(errOut, "scrub-stubs: no stub was scanned; check the source path")
		return 2
	}
	return 0
}
