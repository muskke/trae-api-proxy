package utils

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// LoadEnvFile loads KEY=VALUE pairs from a dotenv file without overwriting
// variables that already exist in the process environment.
func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func parseEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}

	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimSpace(parts[0])
	if key == "" {
		return "", "", false
	}

	value := strings.TrimSpace(parts[1])
	if value == "" {
		return key, "", true
	}

	if value[0] == '\'' {
		if end := strings.Index(value[1:], "'"); end >= 0 {
			return key, value[1 : end+1], true
		}
	}
	if value[0] == '"' {
		if parsed, rest, ok := parseQuoted(value); ok {
			_ = rest // comments after a quoted value are intentionally ignored.
			return key, parsed, true
		}
	}

	// For unquoted values, only treat # as a comment delimiter when it is
	// preceded by whitespace. This keeps tokens such as abc#123 intact.
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
			value = strings.TrimSpace(value[:i])
			break
		}
	}
	return key, value, true
}

func parseQuoted(value string) (parsed, rest string, ok bool) {
	for i := 1; i < len(value); i++ {
		if value[i] != '"' || value[i-1] == '\\' {
			continue
		}
		quoted := value[:i+1]
		unquoted, err := strconv.Unquote(quoted)
		if err != nil {
			return "", "", false
		}
		return unquoted, strings.TrimSpace(value[i+1:]), true
	}
	return "", "", false
}

// EnvOrDefault returns the environment value when it is non-empty, otherwise
// fallback.
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
