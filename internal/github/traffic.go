package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TrafficPoint is one daily bucket of views or clones. GitHub keeps only the
// last 14 days, so a bucket not captured inside that window is lost for good.
type TrafficPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Count     int64     `json:"count"`
	Uniques   int64     `json:"uniques"`
}

// TrafficSeries is the 14-day rolling window for one metric. Only one of
// Views/Clones is populated depending on the endpoint; Points normalises that.
type TrafficSeries struct {
	Count   int64          `json:"count"`
	Uniques int64          `json:"uniques"`
	Views   []TrafficPoint `json:"views"`
	Clones  []TrafficPoint `json:"clones"`
}

// Points returns the daily buckets regardless of which endpoint they came from.
func (s TrafficSeries) Points() []TrafficPoint {
	if len(s.Views) > 0 {
		return s.Views
	}
	return s.Clones
}

// Views returns the 14-day view series. Requires a classic token with the
// public_repo scope; fine-grained tokens are refused by these endpoints.
func (c *Client) Views(ctx context.Context) (TrafficSeries, []byte, error) {
	return c.trafficSeries(ctx, "/repos/"+c.Repo+"/traffic/views")
}

// Clones returns the 14-day clone series. Clone count is the closest available
// proxy for `go install` usage, which has no download counter of its own.
func (c *Client) Clones(ctx context.Context) (TrafficSeries, []byte, error) {
	return c.trafficSeries(ctx, "/repos/"+c.Repo+"/traffic/clones")
}

func (c *Client) trafficSeries(ctx context.Context, path string) (TrafficSeries, []byte, error) {
	raw, err := c.getJSON(ctx, path)
	if err != nil {
		return TrafficSeries{}, nil, err
	}
	var s TrafficSeries
	if err := json.Unmarshal(raw, &s); err != nil {
		return TrafficSeries{}, nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return s, raw, nil
}

// TopItem is one entry of a popular-paths or popular-referrers list. These are
// point-in-time top-10 rankings over the trailing 14 days, not daily series.
type TopItem struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	Referrer string `json:"referrer"`
	Count    int64  `json:"count"`
	Uniques  int64  `json:"uniques"`
}

// Name is the identifying field, whichever kind of list this came from.
func (t TopItem) Name() string {
	if t.Referrer != "" {
		return t.Referrer
	}
	return t.Path
}

// PopularPaths returns the top-10 most viewed repository paths. Note this
// counts github.com HTML page views only — it does not see raw.githubusercontent
// fetches, so `curl | sh` installs of install.sh never appear here.
func (c *Client) PopularPaths(ctx context.Context) ([]TopItem, []byte, error) {
	return c.topList(ctx, "/repos/"+c.Repo+"/traffic/popular/paths")
}

// PopularReferrers returns the top-10 referring sites.
func (c *Client) PopularReferrers(ctx context.Context) ([]TopItem, []byte, error) {
	return c.topList(ctx, "/repos/"+c.Repo+"/traffic/popular/referrers")
}

func (c *Client) topList(ctx context.Context, path string) ([]TopItem, []byte, error) {
	raw, err := c.getJSON(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	var items []TopItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return items, raw, nil
}
