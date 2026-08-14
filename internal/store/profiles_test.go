package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dejo1307/git-stats/internal/github"
)

// cacheProfile writes one profile record into the archive's shared user cache,
// the way a collection run does.
func cacheProfile(t *testing.T, dataDir, login, profile string, fetchedAt time.Time) {
	t.Helper()
	snap := Snapshot{Root: ArchiveDir(dataDir)}
	raw, err := json.Marshal(github.UserRecord{
		Login: login, FetchedAt: fetchedAt, ETag: `"v1"`,
		Profile: json.RawMessage(profile),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.WriteUser(login, raw); err != nil {
		t.Fatalf("caching %s: %v", login, err)
	}
}

const adaProfile = `{"login":"ada","name":"Ada Lovelace","email":"ada@example.org",
	"company":"@analytical","blog":"https://ada.example","location":"London",
	"bio":"notes","twitter_username":"ada","followers":12,"public_repos":7,
	"type":"User","created_at":"2015-01-02T03:04:05Z"}`

// graceProfile is the ordinary case: a real person who publishes nothing but a
// display name. Absent fields must reach SQL as NULL, not as empty strings, or
// "has no public email" stops being a condition a query can express.
const graceProfile = `{"login":"grace","name":"Grace Hopper","email":null,
	"company":null,"blog":"","location":null,"bio":null,"twitter_username":null,
	"followers":3,"public_repos":1,"type":"User","created_at":"2016-05-06T07:08:09Z"}`

const profileStarsJSON = `[{"starred_at":"2026-08-01T00:00:00Z","user":{"login":"ada"}},
	{"starred_at":"2026-08-02T00:00:00Z","user":{"login":"grace"}}]`

func TestProfilesAreDerivedFromTheCacheAndSurviveRebuild(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	cacheProfile(t, dir, "ada", adaProfile, at)
	cacheProfile(t, dir, "grace", graceProfile, at)
	snapshotAt(t, dir, at, map[string]string{FileStargazers: profileStarsJSON})

	assert := func(t *testing.T, when string) {
		t.Helper()
		db, err := Open(DBPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		var name, email, company string
		var fetched string
		if err := db.QueryRow(`SELECT name, email, company, fetched_at
			FROM stargazer_profile WHERE login = 'ada'`).
			Scan(&name, &email, &company, &fetched); err != nil {
			t.Fatalf("%s: reading ada: %v", when, err)
		}
		if name != "Ada Lovelace" || email != "ada@example.org" || company != "@analytical" {
			t.Errorf("%s: ada = %q/%q/%q", when, name, email, company)
		}
		if fetched != at.Format(time.RFC3339) {
			t.Errorf("%s: fetched_at = %q, want the archived fetch time", when, fetched)
		}

		// An unset field is NULL, so counting the reachable stargazers is one
		// IS NOT NULL rather than a string comparison that has to guess.
		var withEmail int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM stargazer_profile WHERE email IS NOT NULL`).
			Scan(&withEmail); err != nil {
			t.Fatal(err)
		}
		if withEmail != 1 {
			t.Errorf("%s: %d profiles with an email, want 1", when, withEmail)
		}
		var blogNull bool
		if err := db.QueryRow(
			`SELECT blog IS NULL FROM stargazer_profile WHERE login = 'grace'`).
			Scan(&blogNull); err != nil {
			t.Fatal(err)
		}
		if !blogNull {
			t.Errorf(`%s: an empty blog must be NULL, not ""`, when)
		}
	}

	assert(t, "after collection")

	// The archive is the source of truth, so throwing the database away and
	// replaying costs derive time and nothing else — including for the profile
	// cache, which is why the fetch time and ETag live in the archived record
	// rather than in a database the rebuild is about to delete.
	if _, err := Rebuild(dir, testNamer); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	assert(t, "after rebuild")
}

func TestStargazerWithoutACachedProfileHasNoRow(t *testing.T) {
	// Not being fetched is the ordinary state: the crawl is opt-in and priced
	// per account. It has to be distinguishable from "fetched, publishes
	// nothing", which is a row full of NULLs.
	dir := t.TempDir()
	at := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	cacheProfile(t, dir, "ada", adaProfile, at)
	snapshotAt(t, dir, at, map[string]string{FileStargazers: profileStarsJSON})

	db, err := Open(DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var stars, profiles int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM stargazer),
		(SELECT COUNT(*) FROM stargazer_profile)`).Scan(&stars, &profiles); err != nil {
		t.Fatal(err)
	}
	if stars != 2 || profiles != 1 {
		t.Errorf("got %d stars / %d profiles, want 2 / 1", stars, profiles)
	}
}

func TestUserPathRefusesALoginThatIsNotOne(t *testing.T) {
	// The login becomes a filename, so a value that did not come from the API
	// must not be turned into a path at all.
	snap := Snapshot{Root: t.TempDir()}
	for _, login := range []string{"../escape", "a/b", "", "with space", "sem;colon"} {
		if _, err := snap.UserPath(login); err == nil {
			t.Errorf("UserPath(%q) was accepted", login)
		}
		if err := snap.WriteUser(login, []byte(`{}`)); err == nil {
			t.Errorf("WriteUser(%q) was accepted", login)
		}
	}
	if _, err := snap.UserPath("Ada-Lovelace-99"); err != nil {
		t.Errorf("a real login was rejected: %v", err)
	}
}

func TestReadUserReportsAbsenceRatherThanFailing(t *testing.T) {
	snap := Snapshot{Root: t.TempDir()}
	rec, found, err := snap.ReadUser("nobody")
	if err != nil || found {
		t.Fatalf("ReadUser on an empty cache = %+v, found %v, err %v", rec, found, err)
	}

	// A corrupt record is a different thing and must be reported, not silently
	// treated as never fetched — that would refetch it forever.
	path := filepath.Join(snap.Root, "users", "broken.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snap.ReadUser("broken"); err == nil {
		t.Error("a corrupt cached record was accepted")
	}
}
