package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// User is one account's public profile.
//
// Every field here is what GitHub shows any signed-in reader on the account's
// profile page — nothing is inferred, derived from commits, or assembled from
// anywhere else. Email in particular is the *publicly displayed* profile email:
// it is opt-in, most accounts leave it unset, and an account that sets it has
// chosen to publish it. It is not the commit author address, which GitHub also
// exposes but which people frequently never intended to make public.
//
// The endpoint needs a credential to be worth calling at all. Unauthenticated
// it still answers 200 and still returns the profile, but with email always
// null — so an unauthenticated crawl silently reports every account as having
// no address rather than failing in a way anyone would notice.
type User struct {
	Login       string    `json:"login"`
	Name        string    `json:"name"`
	Email       string    `json:"email"`
	Company     string    `json:"company"`
	Blog        string    `json:"blog"`
	Location    string    `json:"location"`
	Bio         string    `json:"bio"`
	Twitter     string    `json:"twitter_username"`
	Followers   int64     `json:"followers"`
	PublicRepos int64     `json:"public_repos"`
	CreatedAt   time.Time `json:"created_at"`
	// Type separates people from organisations and bots, both of which can
	// star a repository and neither of which is somebody to ask for feedback.
	Type string `json:"type"`
}

// UserRecord is one archived profile fetch.
//
// The response is kept verbatim in Profile, wrapped in an envelope carrying
// what the response body itself does not say: which login was asked for, when,
// and under which ETag. Storing that beside the bytes rather than in the
// database is what lets a rebuild restore the cache's freshness state, so
// throwing the database away costs nothing but derive time — the rule the rest
// of the archive already follows.
type UserRecord struct {
	Login     string          `json:"login"`
	FetchedAt time.Time       `json:"fetched_at"`
	ETag      string          `json:"etag,omitempty"`
	Profile   json.RawMessage `json:"profile"`
}

// Stale reports whether the record is older than max. A zero max never goes
// stale, which is what makes "fetch only what is missing" the default.
func (r UserRecord) Stale(now time.Time, max time.Duration) bool {
	return max > 0 && now.Sub(r.FetchedAt) >= max
}

// Decode returns the archived profile.
func (r UserRecord) Decode() (User, error) {
	var u User
	if len(r.Profile) == 0 {
		return u, fmt.Errorf("archived profile for %s is empty", r.Login)
	}
	if err := json.Unmarshal(r.Profile, &u); err != nil {
		return u, fmt.Errorf("decoding archived profile for %s: %w", r.Login, err)
	}
	return u, nil
}

// ValidLogin reports whether login is shaped like a GitHub account name.
//
// GitHub allows only letters, digits and hyphens, so anything else means the
// value did not come from where it is assumed to have come from. It is checked
// because a login is used as a filename, and a login containing a separator
// would write outside the cache.
func ValidLogin(login string) bool {
	if login == "" || len(login) > 39 {
		return false
	}
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// UserProfile fetches one account's public profile.
//
// A non-empty etag makes the request conditional: unchanged is true when
// GitHub answers 304, in which case raw is nil and the caller's cached copy is
// still current. Those cost nothing against the rate limit, which is what keeps
// re-checking a large stargazer list affordable.
func (c *Client) UserProfile(ctx context.Context, login, etag string) (
	raw []byte, newETag string, unchanged bool, err error) {

	if !ValidLogin(login) {
		return nil, "", false, fmt.Errorf("invalid login %q", login)
	}
	body, resp, err := c.getWith(ctx, c.base()+"/users/"+login,
		"application/vnd.github+json", etag)
	if err != nil {
		return nil, "", false, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	// Decoded only to reject a body that is not a profile; the bytes archived
	// are the ones received.
	var u User
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, "", false, fmt.Errorf("decoding profile for %s: %w", login, err)
	}
	return body, resp.Header.Get("ETag"), false, nil
}
