package github

import "testing"

func TestAssetNamerParse(t *testing.T) {
	tests := []struct {
		name     string
		wantOK   bool
		version  string
		platform string
		kind     string
	}{
		{"widget-0.3.6-darwin-arm64.tar.gz", true, "0.3.6", "darwin-arm64", "tar.gz"},
		{"widget-0.3.6-darwin-arm64.sha256", true, "0.3.6", "darwin-arm64", "sha256"},
		{"widget-0.1.40-linux-amd64.tar.gz", true, "0.1.40", "linux-amd64", "tar.gz"},
		{"widget-1.0.0-rc1-windows-amd64.zip", true, "1.0.0-rc1", "windows-amd64", "zip"},
		// Anything off-scheme is kept but left unclassified rather than dropped.
		{"checksums.txt", false, "", "", ""},
		{"widget-0.3.6-darwin-arm64.dmg", false, "", "", ""},
		{"othertool-0.3.6-darwin-arm64.tar.gz", false, "", "", ""},
		{"", false, "", "", ""},
	}

	namer := NewAssetNamer("widget")
	for _, tt := range tests {
		got, ok := namer.Parse(tt.name)
		if ok != tt.wantOK {
			t.Errorf("Parse(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Version != tt.version {
			t.Errorf("Parse(%q) version = %q, want %q", tt.name, got.Version, tt.version)
		}
		if got.Platform() != tt.platform {
			t.Errorf("Parse(%q) platform = %q, want %q", tt.name, got.Platform(), tt.platform)
		}
		if got.Kind != tt.kind {
			t.Errorf("Parse(%q) kind = %q, want %q", tt.name, got.Kind, tt.kind)
		}
	}
}

// A prefix with regex metacharacters must be matched literally, and the zero
// namer must classify nothing rather than panic.
func TestAssetNamerEdges(t *testing.T) {
	if _, ok := NewAssetNamer("my.tool").Parse("myXtool-1.0.0-linux-amd64.tar.gz"); ok {
		t.Error("prefix metacharacters were not quoted")
	}
	if _, ok := NewAssetNamer("my.tool").Parse("my.tool-1.0.0-linux-amd64.tar.gz"); !ok {
		t.Error("literal prefix with a dot did not match")
	}
	if _, ok := (AssetNamer{}).Parse("widget-1.0.0-linux-amd64.tar.gz"); ok {
		t.Error("zero AssetNamer classified an asset")
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"owner/widget": "widget",
		"widget":       "widget",
	} {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersion(t *testing.T) {
	if got := Version("v0.3.6"); got != "0.3.6" {
		t.Errorf("Version(v0.3.6) = %q, want 0.3.6", got)
	}
	if got := Version("0.3.6"); got != "0.3.6" {
		t.Errorf("Version(0.3.6) = %q, want 0.3.6", got)
	}
}
