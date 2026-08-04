// Package dotenv loads KEY=VALUE pairs from a .env file into the process
// environment, so a credential can live in a gitignored file instead of a
// shell profile.
//
// Variables already present in the real environment always win: a .env file
// supplies defaults, it does not override an explicit export.
package dotenv

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Filename is the conventional name looked for in each search directory.
const Filename = ".env"

// Loaded describes what a Load call did.
type Loaded struct {
	// Path is the file that was read, empty when none was found.
	Path string
	// Applied lists the variables the file actually set. Variables already
	// present in the environment are not included, so callers can tell a
	// file-supplied value from an exported one.
	Applied map[string]bool
}

// Supplied reports whether the named variable's value came from the file.
func (l Loaded) Supplied(key string) bool { return l.Applied[key] }

// Load reads the first .env file it finds and sets any variable not already
// present in the environment. A missing .env is normal, not an error.
//
// Search order is the working directory first, then the directory holding the
// binary, so the tool finds its own .env however it was invoked.
func Load() (Loaded, error) {
	for _, dir := range searchDirs() {
		path := filepath.Join(dir, Filename)
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Loaded{}, err
		}
		defer f.Close()

		vars, err := Parse(f)
		if err != nil {
			return Loaded{}, fmt.Errorf("%s: %w", path, err)
		}
		loaded := Loaded{Path: path, Applied: map[string]bool{}}
		for k, v := range vars {
			if _, present := os.LookupEnv(k); present {
				continue
			}
			if err := os.Setenv(k, v); err != nil {
				return Loaded{}, err
			}
			loaded.Applied[k] = true
		}
		return loaded, nil
	}
	return Loaded{}, nil
}

func searchDirs() []string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); len(dirs) == 0 || dir != dirs[0] {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// Parse reads KEY=VALUE lines. Blank lines and # comments are ignored, a
// leading "export " is tolerated, and values may be single- or double-quoted.
// Double-quoted values interpret \n, \r, \t and \\; single-quoted values are
// literal. An unquoted value has trailing whitespace and any trailing
// # comment stripped.
func Parse(r io.Reader) (map[string]string, error) {
	vars := map[string]string{}
	scanner := bufio.NewScanner(r)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")

		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", line, text)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", line)
		}
		vars[key] = unquote(strings.TrimSpace(value))
	}
	return vars, scanner.Err()
}

func unquote(v string) string {
	if len(v) >= 2 {
		switch {
		case v[0] == '"' && v[len(v)-1] == '"':
			return expandEscapes(v[1 : len(v)-1])
		case v[0] == '\'' && v[len(v)-1] == '\'':
			return v[1 : len(v)-1]
		}
	}
	// Unquoted: an inline comment ends the value, but only when preceded by
	// whitespace, so a "#" inside a token is not truncated.
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

var escapes = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\\`, `\`)

func expandEscapes(v string) string { return escapes.Replace(v) }
