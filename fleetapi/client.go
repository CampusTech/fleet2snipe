package fleetapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var log = logrus.New()

// SetLogLevel sets the package logger level.
func SetLogLevel(level logrus.Level) { log.SetLevel(level) }

// SetLogFormatter sets the package logger formatter.
func SetLogFormatter(f logrus.Formatter) { log.SetFormatter(f) }

// SetLogOutput sets the package logger output.
func SetLogOutput(w io.Writer) { log.SetOutput(w) }

// ErrNotFound is returned when a host or other resource is missing.
var ErrNotFound = errors.New("fleet: not found")

// Client is a minimal Fleet REST client.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
	// MaxRetries is the number of times to retry on 429/5xx. Default 5.
	MaxRetries int
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithHTTPClient overrides the underlying http.Client. Useful for tests.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// NewClient creates a Fleet client. baseURL should be the root of the Fleet
// instance (e.g. https://fleet.example.com). The /api/v1/fleet prefix is added
// automatically. token must be a bearer token from an api_only user.
func NewClient(baseURL, token string, insecureTLS bool, opts ...Option) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("fleet: baseURL is required")
	}
	if token == "" {
		return nil, errors.New("fleet: token is required")
	}
	baseURL = strings.TrimRight(baseURL, "/")

	tr := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	if insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	c := &Client{
		baseURL: baseURL,
		token:   token,
		hc: &http.Client{
			Timeout:   60 * time.Second,
			Transport: tr,
		},
		MaxRetries: 5,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// do executes a request with retry on 429/5xx, honoring Retry-After.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var resp *http.Response
	var lastErr error

	backoff := 500 * time.Millisecond
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		log.WithFields(logrus.Fields{"method": method, "url": u, "attempt": attempt}).Debug("fleet request")
		resp, lastErr = c.hc.Do(req)
		if lastErr != nil {
			// Network errors are retryable.
			if attempt < c.MaxRetries {
				log.WithError(lastErr).WithField("attempt", attempt).Warn("fleet request failed, retrying")
				if !sleep(ctx, backoff) {
					return nil, ctx.Err()
				}
				backoff *= 2
				continue
			}
			return nil, lastErr
		}

		// 429 or 5xx: respect Retry-After, then back off.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			retryAfter := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					retryAfter = time.Duration(secs) * time.Second
				}
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if attempt < c.MaxRetries {
				log.WithFields(logrus.Fields{
					"status":      resp.StatusCode,
					"retry_after": retryAfter.String(),
					"attempt":     attempt,
				}).Warn("fleet rate-limited / transient error, backing off")
				if !sleep(ctx, retryAfter) {
					return nil, ctx.Err()
				}
				backoff *= 2
				continue
			}
			return nil, fmt.Errorf("fleet: %s %s gave up after %d retries (status %d)", method, path, c.MaxRetries, resp.StatusCode)
		}

		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return resp, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// readJSON reads and decodes a successful response body. 4xx → error.
func readJSON(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fleet api error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version returns Fleet server build info — useful as a connectivity test.
func (c *Client) Version(ctx context.Context) (*Version, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/latest/fleet/version", nil, nil)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Version
	}
	if err := readJSON(resp, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Version, nil
}

// ListHostsOptions configures pagination and which sub-objects to populate.
type ListHostsOptions struct {
	Page             int    // 0-based
	PerPage          int    // capped at 10000 by Fleet
	OrderKey         string // e.g. "id"
	OrderDirection   string // "asc" or "desc"
	Status           string // online/offline/missing/new
	TeamID           int    // 0 = all teams
	Query            string // full-text search
	PopulateSoftware bool   // includes software (without_vulnerability_details)
	PopulateLabels   bool
	PopulateUsers    bool
	PopulatePolicies bool
}

// ListHosts fetches one page of hosts.
func (c *Client) ListHosts(ctx context.Context, opts ListHostsOptions) ([]Host, error) {
	q := url.Values{}
	q.Set("page", strconv.Itoa(opts.Page))
	if opts.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(opts.PerPage))
	}
	if opts.OrderKey != "" {
		q.Set("order_key", opts.OrderKey)
	}
	if opts.OrderDirection != "" {
		q.Set("order_direction", opts.OrderDirection)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.TeamID > 0 {
		q.Set("team_id", strconv.Itoa(opts.TeamID))
	}
	if opts.Query != "" {
		q.Set("query", opts.Query)
	}
	if opts.PopulateSoftware {
		q.Set("populate_software", "without_vulnerability_details")
	}
	if opts.PopulateLabels {
		q.Set("populate_labels", "true")
	}
	if opts.PopulateUsers {
		q.Set("populate_users", "true")
	}
	if opts.PopulatePolicies {
		q.Set("populate_policies", "true")
	}

	resp, err := c.do(ctx, http.MethodGet, "/api/v1/fleet/hosts", q, nil)
	if err != nil {
		return nil, err
	}
	var env listHostsResponse
	if err := readJSON(resp, &env); err != nil {
		return nil, err
	}
	out := make([]Host, 0, len(env.Hosts))
	for _, raw := range env.Hosts {
		var h Host
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("decoding host: %w", err)
		}
		h.Raw = raw
		out = append(out, h)
	}
	return out, nil
}

// ListAllHosts pages through all hosts using opts.PerPage. opts.Page is ignored.
func (c *Client) ListAllHosts(ctx context.Context, opts ListHostsOptions) ([]Host, error) {
	if opts.PerPage <= 0 {
		opts.PerPage = 1000
	}
	if opts.OrderKey == "" {
		opts.OrderKey = "id"
		opts.OrderDirection = "asc"
	}

	var all []Host
	for page := 0; ; page++ {
		opts.Page = page
		batch, err := c.ListHosts(ctx, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		log.WithFields(logrus.Fields{"page": page, "count": len(batch), "total": len(all)}).Debug("fleet hosts page")
		if len(batch) < opts.PerPage {
			break
		}
		if err := ctx.Err(); err != nil {
			return all, err
		}
	}
	return all, nil
}

// GetHost fetches a single host by ID. Includes software, users, policies, etc.
func (c *Client) GetHost(ctx context.Context, id uint) (*Host, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/fleet/hosts/%d", id), nil, nil)
	if err != nil {
		return nil, err
	}
	var env hostDetailResponse
	if err := readJSON(resp, &env); err != nil {
		return nil, err
	}
	var h Host
	if err := json.Unmarshal(env.Host, &h); err != nil {
		return nil, fmt.Errorf("decoding host detail: %w", err)
	}
	h.Raw = env.Host
	return &h, nil
}

// HostByIdentifier looks up a host by uuid, serial, hostname, or node_key.
// Returns ErrNotFound if no host matches.
func (c *Client) HostByIdentifier(ctx context.Context, identifier string) (*Host, error) {
	if identifier == "" {
		return nil, errors.New("fleet: identifier is required")
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/fleet/hosts/identifier/"+url.PathEscape(identifier), nil, nil)
	if err != nil {
		return nil, err
	}
	var env hostDetailResponse
	if err := readJSON(resp, &env); err != nil {
		return nil, err
	}
	var h Host
	if err := json.Unmarshal(env.Host, &h); err != nil {
		return nil, fmt.Errorf("decoding host detail: %w", err)
	}
	h.Raw = env.Host
	return &h, nil
}

// ListLabels returns all labels.
func (c *Client) ListLabels(ctx context.Context) ([]Label, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/fleet/labels", nil, nil)
	if err != nil {
		return nil, err
	}
	var env listLabelsResponse
	if err := readJSON(resp, &env); err != nil {
		return nil, err
	}
	return env.Labels, nil
}

// ListTeams returns all teams.
func (c *Client) ListTeams(ctx context.Context) ([]Team, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/fleet/teams", nil, nil)
	if err != nil {
		if strings.Contains(err.Error(), "status=402") || strings.Contains(err.Error(), "status=403") {
			// Teams are a Premium feature; not having them isn't fatal.
			return nil, nil
		}
		return nil, err
	}
	var env listTeamsResponse
	if err := readJSON(resp, &env); err != nil {
		return nil, err
	}
	return env.Teams, nil
}
