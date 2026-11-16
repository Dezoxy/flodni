package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthTokensFileKeyAndEnv(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"auth_tokens_file":"/etc/flodni/tokens.json"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.AuthTokensFile != "/etc/flodni/tokens.json" {
		t.Fatalf("key not read: %q", c.AuthTokensFile)
	}
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "/run/secrets/tokens.json")
	c.applyEnvOverrides()
	if c.AuthTokensFile != "/run/secrets/tokens.json" {
		t.Fatalf("the variable must win: %q", c.AuthTokensFile)
	}
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "")
	c.AuthTokensFile = "/etc/flodni/tokens.json"
	c.applyEnvOverrides()
	if c.AuthTokensFile != "/etc/flodni/tokens.json" {
		t.Fatalf("an empty variable must leave the file's value: %q", c.AuthTokensFile)
	}
}

// A file with a type error elsewhere still keeps its auth_tokens_file for a save, as
// allowed_hosts does.
func TestFileAuthTokensFileSurvivesAParseError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"auth_tokens_file":"/etc/flodni/t.json","metrics_port":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MKV_PROXY_CONFIG_PATH", p)
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "/env.json")
	c := LoadConfig()
	if got := c.FileAuthTokensFile(); got != "/etc/flodni/t.json" {
		t.Fatalf("file value = %q", got)
	}
	if c.AuthTokensFile != "/env.json" {
		t.Fatalf("running value = %q", c.AuthTokensFile)
	}
}
