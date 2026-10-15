package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := load(func(string) (string, bool) { return "", false }, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("base url %q", cfg.BaseURL)
	}
	if cfg.Model != DefaultModel {
		t.Fatalf("model %q", cfg.Model)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Fatalf("timeout %s", cfg.Timeout)
	}
	if cfg.APIKey != "" {
		t.Fatal("expected empty api key")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantDB := filepath.Join(home, ".atmo-mem", "atmo-mem.db")
	if cfg.DBPath != wantDB {
		t.Fatalf("db path %q want %q", cfg.DBPath, wantDB)
	}
}

func TestLoadEnvBeatsFileAndOpenAIFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "" +
		"# comment\n" +
		"export ATMO_MEM_API_KEY=from-file\n" +
		"ATMO_MEM_MODEL=\"file-model\"\n" +
		"ATMO_MEM_BASE_URL='https://file.example/v1'\n" +
		"ATMO_MEM_TIMEOUT=5s\n" +
		"ATMO_MEM_PROJECT=from-file\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ATMO_MEM_API_KEY": "from-env",
		"OPENAI_MODEL":     "openai-model",
		"OPENAI_BASE_URL":  "https://openai.example/v1/",
	}
	cfg, err := load(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}, path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "from-env" {
		t.Fatalf("api key %q", cfg.APIKey)
	}
	if cfg.Model != "file-model" {
		t.Fatalf("model %q", cfg.Model)
	}
	if cfg.BaseURL != "https://file.example/v1" {
		t.Fatalf("base url %q", cfg.BaseURL)
	}
	if cfg.Timeout != 5*time.Second {
		t.Fatalf("timeout %s", cfg.Timeout)
	}
	if cfg.Project != "from-file" {
		t.Fatalf("project %q", cfg.Project)
	}
}

func TestOpenAIFallbackWhenAtmoUnset(t *testing.T) {
	env := map[string]string{
		"OPENAI_API_KEY":  "sk-openai",
		"OPENAI_BASE_URL": "https://api.openai.com/v1",
		"OPENAI_MODEL":    "gpt-test",
	}
	cfg, err := load(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "sk-openai" || cfg.Model != "gpt-test" || cfg.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("%+v", cfg)
	}
}

func TestEmptyEnvDoesNotBlockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ATMO_MEM_API_KEY=file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(func(k string) (string, bool) {
		if k == "ATMO_MEM_API_KEY" {
			return "  ", true
		}
		return "", false
	}, path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "file-key" {
		t.Fatalf("api key %q", cfg.APIKey)
	}
}

func TestHomeExpansionAndBadTimeout(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := load(func(k string) (string, bool) {
		if k == "ATMO_MEM_DB" {
			return "~/memories/app.db", true
		}
		return "", false
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "memories", "app.db")
	if cfg.DBPath != want {
		t.Fatalf("db %q want %q", cfg.DBPath, want)
	}

	_, err = load(func(k string) (string, bool) {
		if k == "ATMO_MEM_TIMEOUT" {
			return "soon", true
		}
		return "", false
	}, "")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDotEnvRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("NOT A PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := load(func(string) (string, bool) { return "", false }, path)
	if err == nil {
		t.Fatal("expected parse error")
	}
}
