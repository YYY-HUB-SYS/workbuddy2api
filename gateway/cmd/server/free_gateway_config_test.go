package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFreeGatewayConfigDefaultsAndExplicitDisable(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		open, kilo bool
	}{
		{`{}`, true, true},
		{`{"free_gateways":{"opencode":{"enabled":false}}}`, false, true},
		{`{"free_gateways":{"opencode":{"enabled":false},"kilo":{"enabled":false}}}`, false, false},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.FreeGateways.OpenCode.IsEnabled() != tc.open || cfg.FreeGateways.Kilo.IsEnabled() != tc.kilo {
			t.Fatalf("config flag lost: %s", tc.raw)
		}
	}
}
