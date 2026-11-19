package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	content := `
# a comment
export FOO="bar"
BAZ='qux'
PLAIN=value
EMPTYKEY=
=novalue
noequals
  SPACED  =  spaced-val
PRESET=fromfile
`
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// PRESET is already in the environment, so the file must NOT overwrite it.
	t.Setenv("PRESET", "fromenv")
	// Make sure the keys we assert on start unset.
	for _, k := range []string{"FOO", "BAZ", "PLAIN", "EMPTYKEY", "SPACED"} {
		os.Unsetenv(k)
	}

	if err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{
		"FOO":      "bar",
		"BAZ":      "qux",
		"PLAIN":    "value",
		"EMPTYKEY": "",
		"SPACED":   "spaced-val",
		"PRESET":   "fromenv", // not overwritten
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoadMissingFileIsNil(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("Load(missing) = %v, want nil", err)
	}
}

func TestUnquote(t *testing.T) {
	cases := map[string]string{
		`"x"`: "x",
		`'y'`: "y",
		`z`:   "z",
		`"`:   `"`,
		`"a'`: `"a'`, // mismatched quotes: unchanged
		``:    "",
	}
	for in, want := range cases {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}
