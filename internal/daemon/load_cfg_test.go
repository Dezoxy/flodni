package daemon

import (
	"testing"

	"flodni/internal/config"
)

// loadCfg is config.LoadConfig for the tests that read a valid file: any error
// fails the test.
func loadCfg(t *testing.T) config.Config {
	t.Helper()
	c, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return c
}
