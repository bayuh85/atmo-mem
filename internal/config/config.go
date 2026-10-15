// Package config loads atmo-mem settings from the environment and an optional .env file.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultBaseURL is the AtmoRouter OpenAI-compatible endpoint, including /v1.
	DefaultBaseURL = "https://atmorouter.dev/v1"
	// DefaultModel is the chat model used for session summarization.
	DefaultModel = "zai/glm-5.3"
	// DefaultTimeout bounds a single summarization request.
	DefaultTimeout = 60 * time.Second
)

// Config is the process configuration for storage and the LLM client.
type Config struct {
	DBPath  string
	Project string
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

// Load reads .env from the working directory, then the environment.
// Environment values win over .env. ATMO_MEM_* wins over OPENAI_* .
// Empty values are skipped so a blank assignment does not hide a later source.
func Load() (Config, error) {
	return load(os.LookupEnv, ".env")
}

func load(getenv func(string) (string, bool), dotenvPath string) (Config, error) {
	fileVars, err := readDotEnv(dotenvPath)
	if err != nil {
		return Config{}, err
	}
	pick := func(keys ...string) string {
		return firstNonEmpty(getenv, fileVars, keys...)
	}

	dbPath := pick("ATMO_MEM_DB")
	if dbPath == "" {
		dbPath, err = defaultDBPath()
		if err != nil {
			return Config{}, err
		}
	} else {
		dbPath, err = expandHome(dbPath)
		if err != nil {
			return Config{}, err
		}
	}

	timeout := DefaultTimeout
	if raw := pick("ATMO_MEM_TIMEOUT"); raw != "" {
		timeout, err = time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("ATMO_MEM_TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("ATMO_MEM_TIMEOUT must be positive")
		}
	}

	baseURL := pick("ATMO_MEM_BASE_URL", "OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")

	model := pick("ATMO_MEM_MODEL", "OPENAI_MODEL")
	if model == "" {
		model = DefaultModel
	}

	return Config{
		DBPath:  dbPath,
		Project: pick("ATMO_MEM_PROJECT"),
		BaseURL: baseURL,
		APIKey:  pick("ATMO_MEM_API_KEY", "OPENAI_API_KEY"),
		Model:   model,
		Timeout: timeout,
	}, nil
}

// firstNonEmpty walks keys in order. For each key the environment wins over
// .env, and an empty value is skipped so the next source can apply.
// Passing ATMO_MEM_* before OPENAI_* makes the atmo-mem variables win.
func firstNonEmpty(getenv func(string) (string, bool), file map[string]string, keys ...string) string {
	for _, key := range keys {
		if v, ok := getenv(key); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		if v := strings.TrimSpace(file[key]); v != "" {
			return v
		}
	}
	return ""
}

func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".atmo-mem", "atmo-mem.db"), nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~ in database path: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func readDotEnv(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Memory notes and keys are short; allow a long line anyway.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: invalid key %q", path, lineNo, key)
		}
		val, err = unquoteEnv(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 && !(r == '_' || unicode.IsLetter(r)) {
			return false
		}
		if i > 0 && !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

func unquoteEnv(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if v[0] != '"' && v[0] != '\'' {
		return v, nil
	}
	if len(v) < 2 || v[len(v)-1] != v[0] {
		return "", fmt.Errorf("unterminated quoted value")
	}
	return v[1 : len(v)-1], nil
}
