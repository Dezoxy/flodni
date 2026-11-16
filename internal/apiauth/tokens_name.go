package apiauth

// ScopesOf finds the entry called name and returns its scopes. The panel session
// cookie names its token entry, and the gate re-checks it here on every request, so a
// token removed from the file (and the daemon restarted) or demoted cannot keep a
// session alive. Only scoped mode has named entries to find: legacy_token and open
// mode, and a nil store, find nothing. A name is a label, not a secret, so a plain
// comparison is enough.
func (t *Tokens) ScopesOf(name string) (ScopeSet, bool) {
	if t == nil || t.mode != ModeScoped {
		return 0, false
	}
	for i := range t.entries {
		if t.entries[i].Name == name {
			return t.entries[i].Scopes, true
		}
	}
	return 0, false
}
