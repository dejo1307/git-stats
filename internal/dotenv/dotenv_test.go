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

func TestLoadFileReadsTheNamedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enola.env")
	if err := os.WriteFile(path,
		[]byte("GIT_STATS_REPO=owner/named\nGIT_STATS_DIR=/srv/named\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file sitting in the working directory must not win over one the caller
	// named: choosing the file is how one binary tracks two repositories.
	unset(t, "GIT_STATS_REPO")
	unset(t, "GIT_STATS_DIR")

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.Path != path {
		t.Errorf("Path = %q, want %q", loaded.Path, path)
	}
	if got := os.Getenv("GIT_STATS_REPO"); got != "owner/named" {
		t.Errorf("GIT_STATS_REPO = %q, want owner/named", got)
	}
	if !loaded.Supplied("GIT_STATS_DIR") {
		t.Error("GIT_STATS_DIR not reported as file-supplied")
	}
}

// unset removes a variable for the duration of one test, restoring whatever
// the environment held afterwards.
func unset(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // registers the restore
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFileRefusesAMissingFile(t *testing.T) {
	// Load treats absence as normal; LoadFile must not. Falling back to whatever
	// .env is lying around would point the run at another repository and archive
	// its numbers into the wrong directory.
	_, err := LoadFile(filepath.Join(t.TempDir(), "absent.env"))
	if err == nil {
		t.Fatal("LoadFile silently accepted a missing file")
	}
	if !strings.Contains(err.Error(), "absent.env") {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestLoadFileDoesNotOverrideTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.env")
	if err := os.WriteFile(path, []byte("GIT_STATS_REPO=owner/from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_STATS_REPO", "owner/exported")

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := os.Getenv("GIT_STATS_REPO"); got != "owner/exported" {
		t.Errorf("GIT_STATS_REPO = %q, want the exported value to win", got)
	}
	if loaded.Supplied("GIT_STATS_REPO") {
		t.Error("an exported variable was reported as file-supplied")
	}
}
