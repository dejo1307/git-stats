package report_test

import (
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dejo1307/git-stats/internal/report"
	"github.com/dejo1307/git-stats/internal/store"
)

// now is the reference point every -since window in these tests is measured
// from, so the fixtures can be dated relative to it.
var now = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// stargazerDB builds a database holding one star per row given, plus the
// profile behind it when the row has one.
type fixture struct {
	login    string
	daysAgo  int
	profile  string // empty means the account has never been fetched
	forkedBy bool
}

func stargazerDB(t *testing.T, rows ...fixture) *store.DB {
	t.Helper()
	db, err := store.Open(store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	for _, r := range rows {
		starredAt := now.AddDate(0, 0, -r.daysAgo).Format(time.RFC3339)
		if _, err := db.Exec(
			`INSERT INTO stargazer (login, starred_at) VALUES (?, ?)`, r.login, starredAt); err != nil {
			t.Fatal(err)
		}
		if r.profile != "" {
			if _, err := db.Exec(r.profile); err != nil {
				t.Fatalf("seeding the profile for %s: %v", r.login, err)
			}
		}
		if r.forkedBy {
			if _, err := db.Exec(`INSERT INTO fork (full_name, created_at) VALUES (?, ?)`,
				r.login+"/git-stats", starredAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

// profile writes the INSERT for one fetched account. Empty strings become
// NULL, which is how the ingest path stores an unpublished field.
func profile(login, name, email, company, location, accountType string) string {
	col := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return fmt.Sprintf(`INSERT INTO stargazer_profile
		(login, name, email, company, location, account_type, fetched_at)
		VALUES ('%s', %s, %s, %s, %s, %s, '2026-08-14T00:00:00Z')`,
		login, col(name), col(email), col(company), col(location), col(accountType))
}

func fullFixture(t *testing.T) *store.DB {
	t.Helper()
	return stargazerDB(t,
		fixture{login: "ada", daysAgo: 2,
			profile:  profile("ada", "Ada Lovelace", "ada@example.org", "@analytical", "London", "User"),
			forkedBy: true},
		fixture{login: "grace", daysAgo: 10,
			profile: profile("grace", "Grace Hopper", "", "", "New York", "User")},
		fixture{login: "edsger", daysAgo: 40,
			profile: profile("edsger", "Edsger Dijkstra", "edsger@example.net", "@eindhoven", "Nuenen", "User")},
		fixture{login: "acme-corp", daysAgo: 1,
			profile: profile("acme-corp", "Acme", "hello@acme.example", "", "", "Organization")},
		// Starred, never looked up. Not the same as "fetched and publishes
		// nothing", and the difference has to survive every filter.
		fixture{login: "unknown", daysAgo: 3},
	)
}

func run(t *testing.T, db *store.DB, opts report.StargazerOptions) string {
	t.Helper()
	opts.Now = now
	var out strings.Builder
	if err := report.Stargazers(&out, db, opts); err != nil {
		t.Fatalf("Stargazers: %v", err)
	}
	return out.String()
}

func TestStargazersListsNewestFirstWithUnfetchedAccounts(t *testing.T) {
	got := run(t, fullFixture(t), report.StargazerOptions{})
	for _, want := range []string{"ada", "grace", "edsger", "acme-corp", "unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from the default listing:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "acme-corp"), strings.Index(got, "edsger"); i > j {
		t.Errorf("rows are not newest first:\n%s", got)
	}
	// A fork is worth marking: starring and forking is a much stronger signal
	// than starring, and the fork list is already collected.
	if !strings.Contains(got, "ada*") {
		t.Errorf("the fork mark is missing:\n%s", got)
	}
	if !strings.Contains(got, "also forked") {
		t.Errorf("the fork mark is not explained:\n%s", got)
	}
}

func TestStargazerFiltersAreCaseInsensitiveAndAnd(t *testing.T) {
	db := fullFixture(t)
	for _, tc := range []struct {
		name string
		opts report.StargazerOptions
		want []string
		drop []string
	}{
		{"name matches the display name", report.StargazerOptions{Name: "lovelace"},
			[]string{"ada"}, []string{"grace", "edsger"}},
		{"name also matches the login", report.StargazerOptions{Name: "^edsger$"},
			[]string{"edsger"}, []string{"ada"}},
		{"email pattern", report.StargazerOptions{Email: "example.net"},
			[]string{"edsger"}, []string{"ada", "grace"}},
		{"with-email drops the unpublished and the unfetched", report.StargazerOptions{WithEmail: true},
			[]string{"ada", "edsger", "acme-corp"}, []string{"grace", "unknown"}},
		{"company", report.StargazerOptions{Company: "ANALYTICAL"},
			[]string{"ada"}, []string{"edsger"}},
		{"location", report.StargazerOptions{Location: "london"},
			[]string{"ada"}, []string{"grace"}},
		{"people drops organisations", report.StargazerOptions{People: true},
			[]string{"ada", "grace", "unknown"}, []string{"acme-corp"}},
		{"forked", report.StargazerOptions{Forked: true},
			[]string{"ada"}, []string{"grace", "edsger"}},
		{"since", report.StargazerOptions{Since: 7 * 24 * time.Hour},
			[]string{"ada", "acme-corp", "unknown"}, []string{"grace", "edsger"}},
		{"filters AND together", report.StargazerOptions{WithEmail: true, Location: "nuenen"},
			[]string{"edsger"}, []string{"ada", "acme-corp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, db, tc.opts)
			for _, login := range tc.want {
				if !strings.Contains(got, login) {
					t.Errorf("%q was filtered out:\n%s", login, got)
				}
			}
			for _, login := range tc.drop {
				if strings.Contains(got, login) {
					t.Errorf("%q survived the filter:\n%s", login, got)
				}
			}
		})
	}
}

func TestStargazerPatternOnAFieldRequiresHavingOne(t *testing.T) {
	// An account that was never fetched has an empty everything. A pattern like
	// ".*" must not sweep those in — the field is unknown, not matching.
	got := run(t, fullFixture(t), report.StargazerOptions{Company: ".*"})
	if strings.Contains(got, "unknown") {
		t.Errorf("an unfetched account matched a company pattern:\n%s", got)
	}
	if strings.Contains(got, "grace") {
		t.Errorf("an account with no company matched a company pattern:\n%s", got)
	}
	if !strings.Contains(got, "ada") {
		t.Errorf("an account with a company was dropped:\n%s", got)
	}
}

func TestEmailsFormatIsBareAndDeduplicated(t *testing.T) {
	// One person with two accounts is one person; a mail merge fed this list
	// would otherwise write to them twice.
	db := stargazerDB(t,
		fixture{login: "ada", daysAgo: 1,
			profile: profile("ada", "Ada", "ada@example.org", "", "", "User")},
		fixture{login: "ada-alt", daysAgo: 2,
			profile: profile("ada-alt", "Ada", "ADA@example.org", "", "", "User")},
		fixture{login: "grace", daysAgo: 3,
			profile: profile("grace", "Grace", "", "", "", "User")},
	)
	got := run(t, db, report.StargazerOptions{Format: report.FormatEmails, WithEmail: true})

	if got != "ada@example.org\n" {
		t.Errorf("emails output = %q, want one address and nothing else", got)
	}
}

func TestCSVCarriesEveryColumn(t *testing.T) {
	got := run(t, fullFixture(t), report.StargazerOptions{Format: report.FormatCSV})
	records, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("the output is not valid CSV: %v\n%s", err, got)
	}
	if len(records) != 6 {
		t.Fatalf("got %d records, want a header and 5 rows", len(records))
	}
	header := records[0]
	for _, want := range []string{"login", "name", "email", "forked", "starred_at"} {
		if !contains(header, want) {
			t.Errorf("the header is missing %q: %v", want, header)
		}
	}
	// A location holding a comma is the ordinary case and must not split a row.
	db := stargazerDB(t, fixture{login: "ada", daysAgo: 1,
		profile: profile("ada", "Ada", "ada@example.org", "", "Berlin, Germany", "User")})
	records, err = csv.NewReader(strings.NewReader(
		run(t, db, report.StargazerOptions{Format: report.FormatCSV}))).ReadAll()
	if err != nil {
		t.Fatalf("a comma in a field broke the CSV: %v", err)
	}
	if !contains(records[1], "Berlin, Germany") {
		t.Errorf("the location was mangled: %v", records[1])
	}
}

func TestStargazersSaysWhenProfilesAreMissing(t *testing.T) {
	// The two causes of a short contact list need opposite next steps, so the
	// output has to distinguish "the crawl has not finished" from "these people
	// publish nothing".
	partial := stargazerDB(t,
		fixture{login: "ada", daysAgo: 1,
			profile: profile("ada", "Ada", "ada@example.org", "", "", "User")},
		fixture{login: "grace", daysAgo: 2},
	)
	if got := run(t, partial, report.StargazerOptions{}); !strings.Contains(got, "backfill-users") {
		t.Errorf("a partial crawl does not point at the fix:\n%s", got)
	}

	none := stargazerDB(t, fixture{login: "ada", daysAgo: 1})
	if got := run(t, none, report.StargazerOptions{}); !strings.Contains(got, "No profiles have been fetched") {
		t.Errorf("an unfetched list is not called out:\n%s", got)
	}

	complete := stargazerDB(t, fixture{login: "grace", daysAgo: 1,
		profile: profile("grace", "Grace", "", "", "", "User")})
	got := run(t, complete, report.StargazerOptions{})
	if strings.Contains(got, "backfill-users") {
		t.Errorf("a complete crawl still asks for another one:\n%s", got)
	}
	if !strings.Contains(got, "opt-in") {
		t.Errorf("a complete crawl with no addresses does not explain why:\n%s", got)
	}
}

func TestStargazersRejectsABadPatternRatherThanMatchingNothing(t *testing.T) {
	// Silently returning an empty list would read as "nobody matches", which is
	// the same output a correct query can produce.
	err := report.Stargazers(&strings.Builder{}, fullFixture(t),
		report.StargazerOptions{Name: "["})
	if err == nil {
		t.Fatal("an invalid regexp was accepted")
	}
	if !strings.Contains(err.Error(), "-name") {
		t.Errorf("the error does not name the flag: %v", err)
	}
}

func TestStargazersRejectsAnUnknownFormat(t *testing.T) {
	err := report.Stargazers(&strings.Builder{}, fullFixture(t),
		report.StargazerOptions{Format: "json"})
	if err == nil || !strings.Contains(err.Error(), "emails") {
		t.Errorf("an unknown format must list the real ones, got %v", err)
	}
}

func TestEmptyStarHistorySaysSo(t *testing.T) {
	got := run(t, stargazerDB(t), report.StargazerOptions{})
	if !strings.Contains(got, "backfill-stars") {
		t.Errorf("an empty star history does not point at the fix:\n%s", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
