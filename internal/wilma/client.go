// Package wilma implements a minimal client for the unofficial endpoints
// Wilma's (Visma InSchool) own web client uses. There is no official public
// API for guardians. This client only ever issues GET and the two POST
// requests needed to log in and out — it never replies, composes, or
// deletes anything. It does NOT avoid marking messages read: see the
// GET <role-prefix>/messages/<id> note below.
//
// Protocol notes (reverse-engineered against a live "ApiVersion: 30"
// instance; older/other instances may differ):
//
//   - GET /index_json returns a SessionID used once for login.
//   - POST /login redirects (303) to "/?checkcookie" and sets the
//     Wilma2SID cookie on success, or redirects to "/?loginfailed" with no
//     cookie on bad credentials. The response body itself is never JSON.
//   - The authenticated root page ("/") lists each guardian role (child) as
//     <a class="text-style-link" href="/!<id>/">Name...</a>, and carries a
//     "formkey" hidden input needed only for the POST /logout.
//   - GET <role-prefix>/messages/list[/<folder>] returns
//     {"Messages":[...]} JSON — folder is omitted for the default inbox.
//   - GET <role-prefix>/messages/<id> is a server-rendered HTML page (no
//     JSON detail endpoint); the body lives in a
//     <div class="ckeditor hidden">...</div>. Fetching this page marks the
//     message read server-side, the same as opening it in a browser would
//     — there is no side-effect-free way to read a message body.
package wilma

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const userAgent = "wilmabridge/0.1 (+https://github.com/; read-only message sync)"

// ErrSessionExpired is returned when Wilma reports one of its known
// session-expiry error IDs (common-15/18/20/34) instead of the expected
// payload.
var ErrSessionExpired = errors.New("wilma: session expired")

// Role identifies one guardian role (typically one child) within an
// account. Prefix is the path prefix used on role-scoped endpoints, e.g.
// "/!012345"; it is "" for a single-child/no-role account where Wilma never
// exposes a role-selection page.
type Role struct {
	Prefix string
	Name   string
}

// Client is a logged-in (or not-yet-logged-in) session against one Wilma
// host.
type Client struct {
	baseURL *url.URL
	http    *http.Client
	verbose func(format string, args ...any)

	formKey string
}

// NewClient builds a Client for the given host (e.g. "espoo.inschool.fi").
// verbose, if non-nil, receives a line per HTTP request for -v logging; pass
// nil to disable.
func NewClient(host string, verbose func(format string, args ...any)) (*Client, error) {
	if host == "" {
		return nil, errors.New("wilma: empty host")
	}
	base := &url.URL{Scheme: "https", Host: host}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("wilma: cookie jar: %w", err)
	}
	if verbose == nil {
		verbose = func(string, ...any) {}
	}
	return &Client{
		baseURL: base,
		http:    &http.Client{Jar: jar, Timeout: 30 * time.Second},
		verbose: verbose,
	}, nil
}

func (c *Client) url(path string) string {
	u := *c.baseURL
	u.Path = path
	return u.String()
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)
	c.verbose("%s %s", req.Method, req.URL.String())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	c.verbose("  -> %s", resp.Status)
	return resp, nil
}

func (c *Client) get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.url(path), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) postForm(path string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.url(path), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// wilmaError matches Wilma's JSON error envelope, e.g.
// {"error":{"id":"common-15","message":"..."}}.
type wilmaError struct {
	Error struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"error"`
}

var sessionExpiredIDs = map[string]bool{
	"common-15": true,
	"common-18": true,
	"common-20": true,
	"common-34": true,
}

// checkWilmaError inspects a raw JSON body for Wilma's error envelope. It
// returns ErrSessionExpired for known expiry IDs, a generic error for any
// other error ID, and nil if the body carries no error envelope.
func checkWilmaError(body []byte) error {
	var we wilmaError
	if err := json.Unmarshal(body, &we); err != nil || we.Error.ID == "" {
		return nil
	}
	if sessionExpiredIDs[we.Error.ID] {
		return fmt.Errorf("%w (%s: %s)", ErrSessionExpired, we.Error.ID, we.Error.Message)
	}
	return fmt.Errorf("wilma: error %s: %s", we.Error.ID, we.Error.Message)
}

type indexJSONResponse struct {
	SessionID string `json:"SessionID"`
}

// login authenticates with a plain username/password (no MFA, no
// Suomi.fi/strong-identification support). Success/failure is determined by
// whether Wilma set the Wilma2SID session cookie, since /login never
// returns JSON or a stable success body — it 303-redirects to
// "/?checkcookie" (success) or "/?loginfailed" (bad credentials), and Go's
// client follows that redirect automatically.
func (c *Client) login(username, password string) error {
	resp, err := c.get("/index_json")
	if err != nil {
		return fmt.Errorf("wilma: fetching session id: %w", err)
	}
	body, err := readBody(resp)
	if err != nil {
		return fmt.Errorf("wilma: reading session id response: %w", err)
	}
	var idx indexJSONResponse
	if err := json.Unmarshal(body, &idx); err != nil {
		return fmt.Errorf("wilma: unexpected /index_json response (not JSON): %w", err)
	}
	if idx.SessionID == "" {
		return errors.New("wilma: /index_json returned no SessionID")
	}

	form := url.Values{
		"Login":        {username},
		"Password":     {password},
		"SESSIONID":    {idx.SessionID},
		"CompleteJson": {""},
		"format":       {"json"},
	}
	resp, err = c.postForm("/login", form)
	if err != nil {
		return fmt.Errorf("wilma: login request: %w", err)
	}
	if _, err := readBody(resp); err != nil {
		return fmt.Errorf("wilma: reading login response: %w", err)
	}

	if !c.hasSessionCookie() {
		return errors.New("wilma: login failed — check credentials; if your school uses Suomi.fi strong identification or MFA, password login is not supported")
	}
	return nil
}

func (c *Client) hasSessionCookie() bool {
	for _, ck := range c.http.Jar.Cookies(c.baseURL) {
		if ck.Name == "Wilma2SID" && ck.Value != "" {
			return true
		}
	}
	return false
}

var (
	roleLinkPattern      = regexp.MustCompile(`<a class="text-style-link" href="(/![0-9]+)/">\s*([^<]+?)\s*(?:<|$)`)
	rolePrefixPattern    = regexp.MustCompile(`/(!\d+)/`)
	logoutFormKeyPattern = regexp.MustCompile(`name="formkey" value="(passwd:[^"]+)"`)
)

// discoverRoles fetches the authenticated root page and extracts the
// guardian roles (one per child) linked from it, along with the account's
// FormKey (needed only for Logout). If the root page carries no role links
// — a single-child account may land directly inside its one role instead
// of a role-picker — it falls back to scanning for any "/!<id>/" path
// occurring on the page, and finally to a single unnamed role with an
// empty prefix.
func (c *Client) discoverRoles() ([]Role, error) {
	resp, err := c.get("/")
	if err != nil {
		return nil, fmt.Errorf("wilma: fetching root page: %w", err)
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("wilma: reading root page: %w", err)
	}

	if m := logoutFormKeyPattern.FindSubmatch(body); m != nil {
		c.formKey = string(m[1])
	}

	var roles []Role
	seen := map[string]bool{}
	for _, m := range roleLinkPattern.FindAllStringSubmatch(string(body), -1) {
		prefix, name := m[1], strings.TrimSpace(m[2])
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		roles = append(roles, Role{Prefix: prefix, Name: name})
	}
	if len(roles) > 0 {
		return roles, nil
	}

	for _, m := range rolePrefixPattern.FindAllStringSubmatch(string(body), -1) {
		prefix := m[1]
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		roles = append(roles, Role{Prefix: "/" + prefix, Name: ""})
	}
	if len(roles) > 0 {
		return roles, nil
	}

	c.verbose("role discovery: no role links found, falling back to single unnamed role")
	return []Role{{Prefix: "", Name: ""}}, nil
}

// LoginAndDiscoverRoles logs in and returns the discovered roles in one
// step.
func (c *Client) LoginAndDiscoverRoles(username, password string) ([]Role, error) {
	if err := c.login(username, password); err != nil {
		return nil, err
	}
	return c.discoverRoles()
}

// Logout invalidates the session. Safe to call even if role discovery never
// found a FormKey; it is a no-op in that case.
func (c *Client) Logout() error {
	if c.formKey == "" {
		return nil
	}
	resp, err := c.postForm("/logout", url.Values{"formkey": {c.formKey}})
	if err != nil {
		return fmt.Errorf("wilma: logout request: %w", err)
	}
	_, err = readBody(resp)
	return err
}

// GetJSON performs a GET against a role-scoped path (prefix + suffix),
// checks for Wilma's error envelope, and unmarshals the body into v.
func (c *Client) GetJSON(prefix, suffix string, v any) error {
	resp, err := c.get(prefix + suffix)
	if err != nil {
		return err
	}
	body, err := readBody(resp)
	if err != nil {
		return err
	}
	if err := checkWilmaError(body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("wilma: decoding %s%s: %w", prefix, suffix, err)
	}
	return nil
}

// GetHTML performs a GET against a role-scoped path and returns the raw
// HTML body.
func (c *Client) GetHTML(prefix, suffix string) ([]byte, error) {
	resp, err := c.get(prefix + suffix)
	if err != nil {
		return nil, err
	}
	return readBody(resp)
}

// GetRaw performs a GET against an absolute path and returns the raw body
// and status code, for probe/debugging use.
func (c *Client) GetRaw(path string) ([]byte, int, error) {
	resp, err := c.get(path)
	if err != nil {
		return nil, 0, err
	}
	body, err := readBody(resp)
	return body, resp.StatusCode, err
}
