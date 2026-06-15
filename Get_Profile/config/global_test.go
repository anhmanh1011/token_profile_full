package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGlobalInstancesDerivesPoolID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin_token_config_global.json")
	data := `[
		{
			"refresh_token": "secret",
			"tenant_id": "tenant",
			"username": "admin",
			"domain": "example.com",
			"proxy": "127.0.0.1:1080",
			"email_file": "email1.txt",
			"bot_prefix": "bot_p01_"
		}
	]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	instances, err := LoadGlobalInstances(path)
	if err != nil {
		t.Fatalf("LoadGlobalInstances: %v", err)
	}

	if got := instances[0].ResolvedPoolID(); got != "bot_p01" {
		t.Fatalf("ResolvedPoolID = %q, want bot_p01", got)
	}
}

func TestSelectGlobalInstanceRequiresPoolWhenMultiple(t *testing.T) {
	instances := []GlobalInstance{
		{BotPrefix: "bot_p01_", EmailFile: "email1.txt", Domain: "example.com", resolvedPoolID: "bot_p01"},
		{BotPrefix: "bot_p02_", EmailFile: "email2.txt", Domain: "example.com", resolvedPoolID: "bot_p02"},
	}

	if _, err := SelectGlobalInstance(instances, ""); err == nil {
		t.Fatal("SelectGlobalInstance without pool succeeded, want error")
	}

	selected, err := SelectGlobalInstance(instances, "bot_p02")
	if err != nil {
		t.Fatalf("SelectGlobalInstance bot_p02: %v", err)
	}
	if got := selected.ResolvedPoolID(); got != "bot_p02" {
		t.Fatalf("ResolvedPoolID = %q, want bot_p02", got)
	}
}
