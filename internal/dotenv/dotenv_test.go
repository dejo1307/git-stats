package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	input := `
# a comment
GIT_STATS_TOKEN=github_pat_abc123

export QUOTED="hello world"
SINGLE='raw $value'
TRAILING=value   # inline comment
HASH_IN_VALUE=abc#def
ESCAPED="line1\nline2"
EMPTY=
SPACED_KEY = spaced
`
	got, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := map[string]string{
		"GIT_STATS_TOKEN": "github_pat_abc123",
		"QUOTED":          "hello world",
		"SINGLE":          "raw $value",
		"TRAILING":        "value",
		// A "#" without preceding whitespace is part of the value, not a comment.
		"HASH_IN_VALUE": "abc#def",
		"ESCAPED":       "line1\nline2",
		"EMPTY":         "",
		"SPACED_KEY":    "spaced",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d vars, want %d: %v", len(got), len(want), got)
	}
}

func TestParseRejectsMalformedLines(t *testing.T) {
	for _, input := range []string{"NOEQUALS\n", "=novalue\n"} {
		if _, err := Parse(strings.NewReader(input)); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", input)
		}
	}
}

func TestLoadDoesNotOverrideRealEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "FROM_FILE=file-value\nALREADY_SET=file-value\n")
	chdir(t, dir)

	// An explicit export must win over the file — otherwise a one-off
	// `GIT_STATS_TOKEN=... git-stats collect` would be silently ignored.
	t.Setenv("ALREADY_SET", "env-value")

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if filepath.Base(loaded.Path) != Filename {
		t.Errorf("loaded %q, want a %s file", loaded.Path, Filename)
	}
	if got := os.Getenv("ALREADY_SET"); got != "env-value" {
		t.Errorf("ALREADY_SET = %q, want the environment value to win", got)
	}
	if got := os.Getenv("FROM_FILE"); got != "file-value" {
		t.Errorf("FROM_FILE = %q, want file-value", got)
	}
	// Supplied must distinguish a file-provided value from an exported one, so
	// the tool can report where a credential actually came from.
	if !loaded.Supplied("FROM_FILE") {
		t.Error("Supplied(FROM_FILE) = false, want true")
	}
	if loaded.Supplied("ALREADY_SET") {
		t.Error("Supplied(ALREADY_SET) = true, but the environment provided that value")
	}
	// Setenv registers its own cleanup, so the variable this test leaked into
	// the process is unset when it ends.
	t.Setenv("FROM_FILE", "")
}

func TestLoadWithoutFileIsNotAnError(t *testing.T) {
	chdir(t, t.TempDir())

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load with no .env present: %v", err)
	}
	if loaded.Path != "" {
		t.Errorf("loaded %q, want no file found", loaded.Path)
	}
}

func writeEnv(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	// t.Chdir restores the previous directory itself and fails the test if
	// either move fails.
	t.Chdir(dir)
}
