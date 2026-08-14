package report_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dejo1307/git-stats/internal/report"
	"github.com/dejo1307/git-stats/internal/store"
)

// dashboard writes the HTML dashboard and returns it.
func dashboard(t *testing.T, db *store.DB, opts report.Options) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dash.html")
	opts.Repo = "acme/widget"
	opts.Now = now
	if err := report.HTMLFile(path, db, opts); err != nil {
		t.Fatalf("HTMLFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var embedded = regexp.MustCompile(`(?s)id="sg-data">(.*?)</script>`)

// embeddedRows returns the stargazer list the page carries.
func embeddedRows(t *testing.T, page string) []map[string]any {
	t.Helper()
	m := embedded.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the page embeds no stargazer data")
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(m[1]), &rows); err != nil {
		t.Fatalf("the embedded list is not valid JSON: %v\n%s", err, m[1])
	}
	return rows
}

func TestContactsAreOptIn(t *testing.T) {
	// The dashboard is the artefact most likely to be mailed on or dropped in
	// a shared folder, so it must not acquire a contact list by default.
	db := fullFixture(t)
	if page := dashboard(t, db, report.Options{}); strings.Contains(page, "sg-data") ||
		strings.Contains(page, "Who starred this") {
		t.Error("the stargazer list appeared without -contacts")
	}
	if page := dashboard(t, db, report.Options{Contacts: true}); !strings.Contains(page, "sg-data") {
		t.Error("the stargazer list is missing with -contacts")
	}
}

func TestEmbeddedRowsCarryWhatTheTableNeeds(t *testing.T) {
	page := dashboard(t, fullFixture(t), report.Options{Contacts: true})
	rows := embeddedRows(t, page)
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(rows))
	}

	byLogin := map[string]map[string]any{}
	for _, r := range rows {
		byLogin[r["login"].(string)] = r
	}

	if got := byLogin["ada"]; got["email"] != "ada@example.org" || got["forked"] != true {
		t.Errorf("ada = %v", got)
	}
	// An organisation is worth flagging: it stars repositories and has no
	// opinion about them.
	if got := byLogin["acme-corp"]["kind"]; got != "Organization" {
		t.Errorf("acme-corp kind = %v, want Organization", got)
	}
	if _, marked := byLogin["ada"]["kind"]; marked {
		t.Error("an ordinary user should carry no kind, to keep the payload small")
	}
	// "Never looked up" and "looked up, publishes nothing" are a row of dashes
	// each, and only the tag separates them.
	if byLogin["unknown"]["unfetched"] != true {
		t.Errorf("an unfetched account is not marked: %v", byLogin["unknown"])
	}
	if _, marked := byLogin["grace"]["unfetched"]; marked {
		t.Error("a fetched account that publishes nothing was marked unfetched")
	}
	// Empty fields are omitted rather than written as "", which is most of the
	// payload on a real list.
	if _, present := byLogin["grace"]["email"]; present {
		t.Error("an unpublished email was written out instead of omitted")
	}
}

func TestContactsAreCappedNewestFirst(t *testing.T) {
	db := fullFixture(t)
	page := dashboard(t, db, report.Options{Contacts: true, MaxContacts: 2})
	rows := embeddedRows(t, page)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the cap of 2", len(rows))
	}
	// The newest stars are kept: they starred something they have just seen,
	// so they are the ones worth asking.
	if rows[0]["login"] != "acme-corp" || rows[1]["login"] != "ada" {
		t.Errorf("the cap kept the wrong rows: %v, %v", rows[0]["login"], rows[1]["login"])
	}
	// A page that quietly dropped 3 of 5 would read as the whole list.
	if !strings.Contains(page, "most recent are listed here") {
		t.Errorf("the page does not say it truncated:\n%s", caption(t, page))
	}
}

func TestEmbeddedDataCannotCloseItsOwnScriptElement(t *testing.T) {
	// Display names and company fields are chosen by other people. Building
	// this page by concatenation would let one of them run script inside a
	// file the maintainer opens from disk.
	hostile := `</script><script>alert(1)</script>`
	db := stargazerDB(t, fixture{login: "mallory", daysAgo: 1,
		profile: profile("mallory", hostile, "m@example.org", hostile, hostile, "User")})

	page := dashboard(t, db, report.Options{Contacts: true})
	rows := embeddedRows(t, page)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — the payload was cut short", len(rows))
	}
	// It survives as data, exactly as given.
	if rows[0]["name"] != hostile {
		t.Errorf("name = %q, want it preserved verbatim", rows[0]["name"])
	}
	// And it never appears as markup: the escaped form is what is in the file.
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("a stargazer's display name reached the page as live markup")
	}
	if !strings.Contains(page, `</script>`) {
		t.Error("the closing tag was not escaped in the embedded JSON")
	}
}

func TestContactsPageStatesItsLimits(t *testing.T) {
	page := dashboard(t, fullFixture(t), report.Options{Contacts: true})
	for _, want := range []string{
		// The table is paginated in the browser, so a reader without
		// JavaScript has to be told where the list actually is.
		"needs JavaScript",
		"git-stats stargazers",
		// The caption has to distinguish an unfinished crawl from people who
		// publish nothing.
		"backfill-users",
		// And the notes have to say what the file now holds.
		"personal data",
		"unsolicited email",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never says %q", want)
		}
	}
	// The coverage sentence is shared with the terminal listing, where
	// backticks are a quoting convention. In HTML they are literal characters.
	if c := caption(t, page); strings.Contains(c, "`") {
		t.Errorf("markdown backticks leaked into the caption: %s", c)
	}
}

// caption returns the paragraph under the stargazer heading.
func caption(t *testing.T, page string) string {
	t.Helper()
	_, after, found := strings.Cut(page, "<h2>Who starred this</h2>")
	if !found {
		t.Fatal("the page has no stargazer section")
	}
	caption, _, _ := strings.Cut(after, "</p>")
	return caption
}
