package store

import (
	"database/sql"
	"time"
)

// StargazerRow is one star together with whatever public profile has been
// fetched for the account that left it.
//
// The profile half is optional and its absence is meaningful. HasProfile false
// means the account has never been fetched — the crawl is opt-in and priced per
// person, so that is the ordinary state before `backfill-users` runs. An empty
// Email on a row where HasProfile is true means something else entirely: the
// account was fetched and publishes no address. Collapsing the two would turn
// "not looked at yet" into "nobody to contact".
type StargazerRow struct {
	Login     string
	StarredAt time.Time

	HasProfile  bool
	Name        string
	Email       string
	Company     string
	Blog        string
	Location    string
	Bio         string
	Twitter     string
	Followers   int64
	PublicRepos int64
	AccountType string
	CreatedAt   time.Time
	FetchedAt   time.Time

	// Forked reports whether this account also owns a fork of the repository.
	// It costs nothing — the fork list is already collected — and starring and
	// forking is a markedly stronger signal of use than starring alone.
	Forked bool
}

// Person reports whether the account is somebody who could answer a question.
// Organisations and bots star repositories too, and neither has an opinion
// about the software.
func (r StargazerRow) Person() bool {
	return !r.HasProfile || r.AccountType == "" || r.AccountType == "User"
}

// Stargazers returns every star, newest first, left-joined to its profile.
//
// The join is left because the two halves come from different crawls: the star
// list is one paginated walk over the repository, a profile is one request per
// account. A star always exists; the profile behind it may not.
func (db *DB) Stargazers() ([]StargazerRow, error) {
	rows, err := db.Query(`
		SELECT s.login, s.starred_at,
		       p.login IS NOT NULL,
		       COALESCE(p.name, ''), COALESCE(p.email, ''), COALESCE(p.company, ''),
		       COALESCE(p.blog, ''), COALESCE(p.location, ''), COALESCE(p.bio, ''),
		       COALESCE(p.twitter, ''), COALESCE(p.followers, 0),
		       COALESCE(p.public_repos, 0), COALESCE(p.account_type, ''),
		       p.created_at, p.fetched_at,
		       EXISTS (
		         SELECT 1 FROM fork f
		         WHERE substr(f.full_name, 1, instr(f.full_name, '/') - 1)
		               = s.login COLLATE NOCASE
		       )
		FROM stargazer s
		LEFT JOIN stargazer_profile p USING (login)
		ORDER BY s.starred_at DESC, s.login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StargazerRow
	for rows.Next() {
		var r StargazerRow
		var starredAt string
		var createdAt, fetchedAt sql.NullString
		if err := rows.Scan(&r.Login, &starredAt, &r.HasProfile,
			&r.Name, &r.Email, &r.Company, &r.Blog, &r.Location, &r.Bio,
			&r.Twitter, &r.Followers, &r.PublicRepos, &r.AccountType,
			&createdAt, &fetchedAt, &r.Forked); err != nil {
			return nil, err
		}
		r.StarredAt, _ = time.Parse(time.RFC3339, starredAt)
		if createdAt.Valid {
			r.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		}
		if fetchedAt.Valid {
			r.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAt.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProfileCoverage counts how much of the star list has been looked up and how
// much of that turned out to be reachable.
//
// Both halves are needed to read a contact list honestly. A small number of
// addresses can mean the crawl has barely started or that the stargazers
// simply do not publish any, and those call for opposite next steps.
type ProfileCoverage struct {
	Stars     int
	Profiles  int
	WithEmail int
}

// ProfileCoverage returns the counts.
func (db *DB) ProfileCoverage() (ProfileCoverage, error) {
	var c ProfileCoverage
	err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM stargazer),
		(SELECT COUNT(*) FROM stargazer_profile),
		(SELECT COUNT(*) FROM stargazer_profile WHERE email IS NOT NULL)`).
		Scan(&c.Stars, &c.Profiles, &c.WithEmail)
	return c, err
}
