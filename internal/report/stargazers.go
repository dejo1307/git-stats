package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dejo1307/git-stats/internal/store"
)

// Output formats for the stargazer list.
const (
	// FormatTable is the aligned terminal listing.
	FormatTable = "table"
	// FormatCSV is the whole record, for a spreadsheet or a mail merge.
	FormatCSV = "csv"
	// FormatEmails is bare addresses, one per line, deduplicated — the form
	// that pipes into something else without further cutting.
	FormatEmails = "emails"
)

// StargazerOptions selects and formats the stargazer list.
//
// The four pattern fields are regular expressions, matched case-insensitively
// and ANDed together. A pattern on a profile field only ever matches an account
// whose profile has been fetched and whose field is non-empty, so filtering on
// a field is also filtering for having one. Name is the exception: it matches
// the login as well as the display name, and a login is the one name every
// account has whether or not it has been looked up.
type StargazerOptions struct {
	Name     string
	Email    string
	Company  string
	Location string
	// WithEmail keeps only accounts that publish an address.
	WithEmail bool
	// Forked keeps only accounts that also forked the repository.
	Forked bool
	// People drops organisations and bots.
	People bool
	// Since keeps only stars given inside the trailing window. Zero is all of
	// them.
	Since time.Duration
	// Limit caps the rows written. Zero is uncapped.
	Limit int
	// Format is one of FormatTable, FormatCSV, FormatEmails.
	Format string
	// Now is the reference point for Since; zero means time.Now.
	Now time.Time
}

func (o StargazerOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// stargazerFilter is the compiled form of the options' patterns.
type stargazerFilter struct {
	opts                           StargazerOptions
	name, email, company, location *regexp.Regexp
	cutoff                         time.Time
}

// compileStargazerFilter validates the patterns up front, so a typo in one is
// an error rather than a silently empty list.
func compileStargazerFilter(opts StargazerOptions) (stargazerFilter, error) {
	f := stargazerFilter{opts: opts}
	for _, spec := range []struct {
		flag    string
		pattern string
		dst     **regexp.Regexp
	}{
		{"-name", opts.Name, &f.name},
		{"-email", opts.Email, &f.email},
		{"-company", opts.Company, &f.company},
		{"-location", opts.Location, &f.location},
	} {
		if spec.pattern == "" {
			continue
		}
		// Case-insensitive by default: these are names and places, where
		// nobody means the match to turn on how somebody capitalised it.
		re, err := regexp.Compile("(?i)" + spec.pattern)
		if err != nil {
			return f, fmt.Errorf("invalid %s pattern %q: %w", spec.flag, spec.pattern, err)
		}
		*spec.dst = re
	}
	if opts.Since > 0 {
		f.cutoff = opts.now().Add(-opts.Since)
	}
	return f, nil
}

// keep reports whether one row survives every filter.
func (f stargazerFilter) keep(r store.StargazerRow) bool {
	switch {
	case !f.cutoff.IsZero() && r.StarredAt.Before(f.cutoff):
		return false
	case f.opts.WithEmail && r.Email == "":
		return false
	case f.opts.Forked && !r.Forked:
		return false
	case f.opts.People && !r.Person():
		return false
	}
	return matches(f.name, r.Name, r.Login) &&
		matches(f.email, r.Email) &&
		matches(f.company, r.Company) &&
		matches(f.location, r.Location)
}

// matches reports whether any candidate satisfies re. A nil re is no filter at
// all and matches everything.
//
// Each candidate is tested on its own rather than against the lot joined
// together, so "^edsger$" anchors to one value instead of to a concatenation
// it could never span. Empty candidates are skipped, which is what makes a
// pattern on a field also a filter for having one: an account that publishes
// no company does not match "-company .*", because the field is unknown rather
// than empty-and-matching.
func matches(re *regexp.Regexp, candidates ...string) bool {
	if re == nil {
		return true
	}
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" && re.MatchString(c) {
			return true
		}
	}
	return false
}

// contactRow is one stargazer as the dashboard embeds it.
//
// Every optional field is omitted when empty rather than written as "". Most
// people publish almost nothing, so on a real list that is most of the bytes —
// which is what keeps a page holding thousands of stargazers small enough to
// open from disk.
type contactRow struct {
	Login     string `json:"login"`
	Starred   string `json:"starred"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	Company   string `json:"company,omitempty"`
	Location  string `json:"location,omitempty"`
	Blog      string `json:"blog,omitempty"`
	Twitter   string `json:"twitter,omitempty"`
	Followers int64  `json:"followers,omitempty"`
	Forked    bool   `json:"forked,omitempty"`
	// Kind marks an account that is not a person. "User" is the overwhelming
	// majority and is left out; an Organization or Bot is worth flagging,
	// because neither has an opinion about the software.
	Kind string `json:"kind,omitempty"`
	// Unfetched marks a star whose account has never been looked up. Without
	// it, such a row is a line of dashes indistinguishable from an account
	// that was fetched and publishes nothing — and the two call for opposite
	// conclusions: one is a gap in the crawl, the other is the answer.
	Unfetched bool `json:"unfetched,omitempty"`
}

// contactData is the stargazer list and what has to be said about it.
type contactData struct {
	Rows []contactRow
	// Total is how many stargazers exist, which differs from len(Rows) when
	// the cap truncated the list.
	Total     int
	Withheld  int
	Coverage  store.ProfileCoverage
	WithEmail int
}

// buildContacts assembles the stargazer list for the dashboard, newest first.
func buildContacts(db *store.DB, opts Options) (contactData, error) {
	var d contactData

	all, err := db.Stargazers()
	if err != nil {
		return d, err
	}
	if d.Coverage, err = db.ProfileCoverage(); err != nil {
		return d, err
	}
	d.Total = len(all)

	limit := opts.MaxContacts
	if limit <= 0 {
		limit = defaultMaxContacts
	}
	if len(all) > limit {
		// Newest first already, so truncating keeps the recent stars — the ones
		// worth asking, since they starred something they have just seen.
		d.Withheld = len(all) - limit
		all = all[:limit]
	}

	d.Rows = make([]contactRow, 0, len(all))
	for _, r := range all {
		row := contactRow{
			Login: r.Login, Starred: r.StarredAt.Format("2006-01-02"),
			Name: r.Name, Email: r.Email, Company: r.Company,
			Location: r.Location, Blog: r.Blog, Twitter: r.Twitter,
			Followers: r.Followers, Forked: r.Forked, Unfetched: !r.HasProfile,
		}
		if r.AccountType != "" && r.AccountType != "User" {
			row.Kind = r.AccountType
		}
		if r.Email != "" {
			d.WithEmail++
		}
		d.Rows = append(d.Rows, row)
	}
	return d, nil
}

// JSON renders the rows for embedding.
//
// template.JS rather than template.HTML: the value lands inside a script
// element, where the escaper works in JavaScript context and would otherwise
// render the whole array as one quoted string literal.
//
// Marking it trusted is safe because encoding/json escapes <, > and & as
// <, > and &, so no value in here — a display name somebody
// chose, say — can close the script element it sits inside. The page reads it
// back with JSON.parse and writes every cell with textContent, so nothing in
// it is ever interpreted as markup either.
func (d contactData) JSON() (template.JS, error) {
	if d.Rows == nil {
		d.Rows = []contactRow{}
	}
	raw, err := json.Marshal(d.Rows)
	if err != nil {
		return "", err
	}
	return template.JS(raw), nil //nolint:gosec // escaped above; see the doc comment
}

// Caption is the paragraph above the table: what the list is, how much of it
// has been looked up, and what it is not licence to do.
func (d contactData) Caption() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d stargazer(s)", d.Coverage.Stars)
	if d.Withheld > 0 {
		fmt.Fprintf(&b, ", of which the %d most recent are listed here", len(d.Rows))
	}
	b.WriteString(". ")
	b.WriteString(coverageLine(d.Coverage))
	b.WriteString(" Everything shown is what GitHub displays to any signed-in visitor " +
		"on the profile page — nothing is inferred, and commit author addresses are " +
		"deliberately not collected.")
	return b.String()
}

// Stargazers writes the filtered stargazer list.
func Stargazers(w io.Writer, db *store.DB, opts StargazerOptions) error {
	filter, err := compileStargazerFilter(opts)
	if err != nil {
		return err
	}

	all, err := db.Stargazers()
	if err != nil {
		return err
	}
	coverage, err := db.ProfileCoverage()
	if err != nil {
		return err
	}

	matched := make([]store.StargazerRow, 0, len(all))
	for _, r := range all {
		if filter.keep(r) {
			matched = append(matched, r)
		}
	}
	shown := matched
	if opts.Limit > 0 && len(shown) > opts.Limit {
		shown = shown[:opts.Limit]
	}

	switch opts.Format {
	case FormatEmails:
		return writeEmails(w, shown)
	case FormatCSV:
		return writeStargazerCSV(w, shown)
	case "", FormatTable:
		return writeStargazerTable(w, shown, matched, coverage)
	default:
		return fmt.Errorf("unknown -format %q: want %s, %s or %s",
			opts.Format, FormatTable, FormatCSV, FormatEmails)
	}
}

// writeEmails writes bare addresses, one per line.
//
// Deduplicated, because one person with two accounts is one person, and a
// mail merge fed this list would otherwise write to them twice. Nothing else
// is printed — not a header, not a count — so the output can be piped straight
// into whatever sends the mail.
func writeEmails(w io.Writer, rows []store.StargazerRow) error {
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		address := strings.ToLower(strings.TrimSpace(r.Email))
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true
		if _, err := fmt.Fprintln(w, r.Email); err != nil {
			return err
		}
	}
	return nil
}

// stargazerCSVHeader is the full record, including the columns a mail merge
// needs to address somebody by name.
var stargazerCSVHeader = []string{
	"login", "starred_at", "name", "email", "company", "blog", "location",
	"twitter", "followers", "public_repos", "account_type", "forked", "profile_fetched_at",
}

func writeStargazerCSV(w io.Writer, rows []store.StargazerRow) error {
	out := csv.NewWriter(w)
	if err := out.Write(stargazerCSVHeader); err != nil {
		return err
	}
	for _, r := range rows {
		fetched := ""
		if !r.FetchedAt.IsZero() {
			fetched = r.FetchedAt.Format(time.RFC3339)
		}
		if err := out.Write([]string{
			r.Login, r.StarredAt.Format(time.RFC3339), r.Name, r.Email, r.Company,
			r.Blog, r.Location, r.Twitter,
			strconv.FormatInt(r.Followers, 10), strconv.FormatInt(r.PublicRepos, 10),
			r.AccountType, strconv.FormatBool(r.Forked), fetched,
		}); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

func writeStargazerTable(w io.Writer, shown, matched []store.StargazerRow,
	coverage store.ProfileCoverage) error {

	if coverage.Stars == 0 {
		fmt.Fprintln(w, "No star history yet. Run `git-stats backfill-stars` first.")
		return nil
	}
	if len(matched) == 0 {
		fmt.Fprintln(w, "No stargazer matches those filters.")
		fmt.Fprintln(w, coverageLine(coverage))
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "starred\tlogin\tname\temail\tcompany\tlocation\t")
	for _, r := range shown {
		fmt.Fprintf(tw, "%s\t%s%s\t%s\t%s\t%s\t%s\t\n",
			r.StarredAt.Format("2006-01-02"), r.Login, forkMark(r),
			cell(r.Name), cell(r.Email), cell(r.Company), cell(r.Location))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n%d of %d stargazer(s) match", len(matched), coverage.Stars)
	if len(shown) < len(matched) {
		fmt.Fprintf(w, ", showing the first %d", len(shown))
	}
	fmt.Fprintf(w, "; %d publish an email.\n", countWithEmail(matched))
	fmt.Fprintln(w, coverageLine(coverage))
	if anyForked(matched) {
		fmt.Fprintln(w, "* also forked the repository.")
	}
	fmt.Fprintln(w, "Add -format emails for addresses alone, or -format csv for every column.")
	return nil
}

// coverageLine states how much of the star list has been looked up, because a
// short contact list has two very different causes — a crawl that has not
// finished, and stargazers who publish nothing — and the reader's next step is
// the opposite in each case.
//
// When the crawl is complete it says so without repeating the address count
// the caller has already printed: the only thing left to explain is that the
// missing addresses are not missing from the archive but from GitHub.
//
// No backticks: this same sentence is also the dashboard's caption, where a
// markdown quoting convention would render as the literal character.
func coverageLine(c store.ProfileCoverage) string {
	switch {
	case c.Profiles == 0:
		return "No profiles have been fetched yet, so every name and email is blank. " +
			"Run git-stats backfill-users."
	case c.Profiles < c.Stars:
		return fmt.Sprintf(
			"Only %d of %d profiles have been fetched, so this list is partial. Run "+
				"git-stats backfill-users again for the rest — it resumes where it stopped.",
			c.Profiles, c.Stars)
	default:
		return "All profiles fetched. The accounts without an address publish none: " +
			"the profile email is opt-in and most people leave it unset."
	}
}

// cell renders an empty profile field as a dash, so a blank column reads as
// "nothing published" rather than as a formatting accident.
func cell(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return strings.Join(strings.Fields(s), " ")
}

func forkMark(r store.StargazerRow) string {
	if r.Forked {
		return "*"
	}
	return ""
}

func countWithEmail(rows []store.StargazerRow) int {
	n := 0
	for _, r := range rows {
		if r.Email != "" {
			n++
		}
	}
	return n
}

func anyForked(rows []store.StargazerRow) bool {
	for _, r := range rows {
		if r.Forked {
			return true
		}
	}
	return false
}
