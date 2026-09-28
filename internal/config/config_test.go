package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestINI(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.cfg")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing test ini: %v", err)
	}
	return path
}

func TestLoadBasics(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
data-listen = :7142
log-level = debug
`)

	cfg, err := Load(path, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":7141" {
		t.Errorf("Listen = %q, want :7141", cfg.Listen)
	}
	if cfg.DataListen != ":7142" {
		t.Errorf("DataListen = %q, want :7142", cfg.DataListen)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestLoadExplicitSection(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141

[SRACacheAlt]
listen = :9999
`)

	cfg, err := Load(path, "SRACacheAlt")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("Listen = %q, want :9999", cfg.Listen)
	}
}

func TestLoadNullConvention(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
download-secret = null
`)

	cfg, err := Load(path, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Raw["download-secret"]; got != "" {
		t.Errorf("Raw[download-secret] = %q, want empty (the \"null\" convention)", got)
	}
}

func TestLoadMissingSection(t *testing.T) {
	path := writeTestINI(t, `
[SomeOtherService]
listen = :1
`)

	if _, err := Load(path, ""); err == nil {
		t.Fatal("Load: expected an error for a missing [SRACache] section, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.cfg"), ""); err == nil {
		t.Fatal("Load: expected an error for a missing file, got nil")
	}
}
