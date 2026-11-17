// Package dotenv loads KEY=VALUE pairs from a .env file into the process
// environment. It is intentionally tiny and dependency-free: enough to keep
// short-lived TURN credentials out of shell history and CLI arguments, not a
// full dotenv implementation.
//
// Rules: blank lines and lines starting with '#' are ignored; an optional
// leading "export " is stripped; surrounding single or double quotes around the
// value are removed; a variable already present in the environment is NOT
// overwritten, so real env vars win over the file.
package dotenv

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// Load reads path and sets each KEY=VALUE into the environment if KEY is not
// already set. A missing file is not an error (returns nil), so callers can
// call Load(".env") unconditionally.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.TrimSpace(val)
		val = unquote(val)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return sc.Err()
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
