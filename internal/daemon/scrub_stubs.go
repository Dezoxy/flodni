package daemon

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"flodni/internal/config"
	"flodni/internal/library"
)

// scrubStubsMain is `flodni scrub-stubs [--dry-run] [source_path]`: the owner-run
// pass that rewrites the magnet of every existing stub to the bare info hash, so a
// tracker passkey an older version wrote is gone from disk. It starts no daemon and
// touches no mount; it walks <source_path>/movies and <source_path>/tv, the stub tree
// the library uses. It prints one summary line and never a magnet or a URL.
func scrubStubsMain(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("scrub-stubs", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dryRun := fs.Bool("dry-run", false, "count the stubs that would be rewritten, change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	source := fs.Arg(0)
	if source == "" {
		log.SetOutput(io.Discard) // LoadConfig logs through the standard logger
		source = config.LoadConfig().PhysicalSourcePath
	}
	if fi, err := os.Stat(source); source == "" || err != nil || !fi.IsDir() {
		_, _ = fmt.Fprintln(errOut, "usage: flodni scrub-stubs [--dry-run] <source_path>")
		_, _ = fmt.Fprintln(errOut, "  source_path is the directory the daemon is given as its source (it holds movies/ and tv/)")
		return 2
	}

	res := library.ScrubStubs([]string{filepath.Join(source, "movies"), filepath.Join(source, "tv")}, *dryRun)
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
	return 0
}
