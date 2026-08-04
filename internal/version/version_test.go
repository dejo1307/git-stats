package version

import "testing"

// An injected version wins over anything the toolchain recorded — that is the
// path every published release takes.
func TestStringPrefersInjectedVersion(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })

	Version = "1.2.3"
	if got := String(); got != "1.2.3" {
		t.Errorf("String() = %q, want 1.2.3", got)
	}
}

// With nothing injected, String must still name something — never an empty
// string, and never a leading "v" that would not match a release tag's assets.
func TestStringFallsBack(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })

	Version = ""
	got := String()
	if got == "" {
		t.Fatal("String() = empty, want a version or \"dev\"")
	}
	if got[0] == 'v' {
		t.Errorf("String() = %q, want no leading \"v\"", got)
	}
}
