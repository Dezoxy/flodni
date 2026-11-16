package daemon

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"flodni/internal/apiauth"
)

const authUsage = `usage: flodni auth new-token --name NAME --scopes a,b
  NAME is [a-z0-9][a-z0-9_-]{0,31}; scopes: admin, engine, library, metrics, read, scrape, upload, webhook.
  Prints the token once (line 1 of stdout) and the JSON entry to add to the "tokens" array of the
  tokens file (line 2). It writes no file and starts no daemon.`

// authMain is `flodni auth`: owner-run, offline. new-token makes a token from 32 bytes of
// crypto/rand and prints it to stdout, once, with the entry that holds its hash. The
// token is not logged and not written anywhere. Exit 0 on success, 2 for a bad
// invocation (nothing is printed to stdout then), 1 if no random bytes could be read.
func authMain(args []string, out, errOut io.Writer) int {
	usage := func(why string) int {
		_, _ = fmt.Fprintf(errOut, "auth: %s\n%s\n", why, authUsage)
		return 2
	}
	if len(args) == 0 || args[0] != "new-token" {
		return usage("a subcommand is required: new-token")
	}
	fs := flag.NewFlagSet("auth new-token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "token name")
	scopes := fs.String("scopes", "", "comma-separated scopes")
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	if fs.NArg() != 0 {
		return usage("unexpected argument " + fmt.Sprintf("%q", fs.Arg(0)))
	}
	if err := apiauth.ValidName(*name); err != nil {
		return usage(err.Error())
	}
	set, err := apiauth.ParseScopes(*scopes)
	if err != nil {
		return usage("--scopes: " + err.Error())
	}
	token, hash, err := apiauth.GenerateToken()
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "auth: no random bytes: %v\n", err)
		return 1
	}
	entry, err := json.Marshal(apiauth.FileEntry{Name: *name, Scopes: set.Names(), SHA256: hash})
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "auth: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "%s\n%s\n", token, entry)
	_, _ = fmt.Fprintln(errOut, "Line 1 of stdout is the token: store it now, it is not shown again and Flodni keeps only its hash.")
	_, _ = fmt.Fprintln(errOut, `Line 2 is the entry: add it to the "tokens" array of auth_tokens_file, then restart.`)
	return 0
}
