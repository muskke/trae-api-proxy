package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		key   string
		value string
		ok    bool
	}{
		{name: "comment", line: "  # hello", ok: false},
		{name: "export", line: "export PORT=9000", key: "PORT", value: "9000", ok: true},
		{name: "quoted comment", line: `TRAE_LOCALE="zh-cn" # en-us`, key: "TRAE_LOCALE", value: "zh-cn", ok: true},
		{name: "unquoted comment", line: "A=value # note", key: "A", value: "value", ok: true},
		{name: "hash in token", line: "TOKEN=abc#123", key: "TOKEN", value: "abc#123", ok: true},
		{name: "single quote", line: "A='value with spaces' # note", key: "A", value: "value with spaces", ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, ok := parseEnvLine(tt.line)
			if key != tt.key || value != tt.value || ok != tt.ok {
				t.Fatalf("parseEnvLine(%q) = %q, %q, %v; want %q, %q, %v", tt.line, key, value, ok, tt.key, tt.value, tt.ok)
			}
		})
	}
}

func TestLoadEnvFileDoesNotOverrideProcessEnvironment(t *testing.T) {
	const key = "TRAE_PROXY_TEST_ENV"
	t.Setenv(key, "from-process")

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(key+"=from-file\nTRAE_PROXY_TEST_NEW=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("TRAE_PROXY_TEST_NEW")

	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "from-process" {
		t.Fatalf("existing environment was overwritten: %q", got)
	}
	if got := os.Getenv("TRAE_PROXY_TEST_NEW"); got != "loaded" {
		t.Fatalf("new variable was not loaded: %q", got)
	}
}
