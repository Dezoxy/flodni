package daemon

import "flodni/internal/config"

// loadCfg is config.LoadConfig for the tests that read a valid file.
func loadCfg() config.Config {
	c, _ := config.LoadConfig()
	return c
}
