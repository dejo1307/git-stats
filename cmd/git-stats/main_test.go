package main

import "testing"

// TestEnvFileArg covers the hand-rolled scan that has to happen before the
// FlagSet exists, because every other flag's default comes from the file it
// names.
func TestEnvFileArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"absent", []string{"-repo", "o/r"}, ""},
		{"single dash, separate", []string{"-env", "a.env", "-repo", "o/r"}, "a.env"},
		{"double dash, separate", []string{"--env", "a.env"}, "a.env"},
		{"single dash, joined", []string{"-env=a.env"}, "a.env"},
		{"double dash, joined", []string{"--env=a.env"}, "a.env"},
		{"after other flags", []string{"-repo", "o/r", "-env", "b.env"}, "b.env"},
		// A trailing -env with nothing after it is the FlagSet's error to
		// report, not this scan's to guess at.
		{"no value", []string{"-env"}, ""},
		{"only the flag name", []string{"-repo", "-env"}, ""},
		// The scan is positional and does not know other flags' arity, so the
		// first -env token wins. Two of them is a mistake either way, and the
		// FlagSet reports the one it sees.
		{"repeated", []string{"-env", "a.env", "-env", "b.env"}, "a.env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := envFileArg(tt.args); got != tt.want {
				t.Errorf("envFileArg(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
