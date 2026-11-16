package config

import (
	"encoding/json"
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
