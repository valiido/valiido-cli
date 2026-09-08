// Package cli implements the Valiido CLI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const clientID = "valiido-cli"
const version = "0.1.0"
const agentScope = "isms:read isms:write"
const legacyReadScope = "documents:read tasks:read risks:read"
const legacyFullScope = "documents:read documents:write tasks:read tasks:write risks:read risks:write"
const (
	maxJSON           = 16 << 20
	pageSize          = 50
	maxListItems      = 10000
	maxCursorBytes    = 4096
	maxTokenBytes     = 512
	maxPairingURI     = 4096
	maxPairingSeconds = 15 * 60
	maxAccessSeconds  = 60 * 60
)

var (
	tenantPattern      = regexp.MustCompile(`^[0-9]{8}$`)
	opaqueTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

func failure(code, message string) error { return &apiError{Code: code, Message: message} }

type identity struct {
	TenantNumber string `json:"tenant_number"`
	AgentName    string `json:"agent_name"`
	Scope        string `json:"scope"`
}

type document struct {
	ID           int64  `json:"id"`
	Version      int64  `json:"version"`
	Title        string `json:"title"`
	Kind         string `json:"kind"`
	ParentID     *int64 `json:"parent_id"`
	UpdatedAt    string `json:"updated_at"`
	ContentText  string `json:"content_text,omitempty"`
	ContentState string `json:"content_state,omitempty"`
}

type resource struct {
	Type       string         `json:"type"`
	ID         int64          `json:"id"`
	Version    *int64         `json:"version"`
	Label      string         `json:"label"`
	Attributes map[string]any `json:"attributes"`
	URL        string         `json:"url,omitempty"`
}

type resourcePage struct {
	Type       string     `json:"type"`
	Resources  []resource `json:"resources"`
	NextCursor string     `json:"next_cursor"`
	Complete   bool       `json:"complete"`
}

type example struct {
	ExampleID  int64          `json:"example_id"`
	Label      string         `json:"label"`
	Copied     bool           `json:"copied"`
	Attributes map[string]any `json:"attributes"`
}

type examplePage struct {
	Type       string    `json:"type"`
	Examples   []example `json:"examples"`
	NextCursor string    `json:"next_cursor"`
	Complete   bool      `json:"complete"`
}

type commandCatalog struct {
	Commands []map[string]any `json:"commands"`
}

type commandResult struct {
	OK       bool           `json:"ok"`
	Data     map[string]any `json:"data"`
	Summary  string         `json:"summary"`
	Warnings []string       `json:"warnings"`
	DryRun   bool           `json:"dry_run,omitempty"`
	Error    *apiError      `json:"error,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type deviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

type client struct {
	base      string
	token     string
	expiresAt time.Time
	refresh   func(context.Context, string) (string, time.Time, error)
	retryLog  io.Writer
	http      *http.Client
}

func newClient(server, token string) (*client, error) {
	if len(server) > 2048 {
		return nil, failure("usage", "Server origin cannot exceed 2048 bytes.")
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, failure("usage", "Server must be an HTTPS origin without a path or credentials.")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, failure("usage", "HTTPS is required; HTTP is allowed only for loopback development.")
	}
	u.Path = ""
	return &client{base: u.String(), token: token, http: &http.Client{
		Timeout: 30 * time.Second,
		// Never follow an API redirect, including to another tenant or host.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// maxRateLimitRetries bounds how many times a single request is transparently
// retried after HTTP 429. The CLI paginates lists and batches imports itself, so
// a large operation can legitimately exceed the server's per-minute budget;
// backing off keeps those operations correct instead of aborting mid-way.
const maxRateLimitRetries = 6

// retryBackoffBase is the first backoff step; each retry doubles it up to a cap.
// A package var (not a const) so tests can set it to zero and run instantly.
var retryBackoffBase = time.Second

func (c *client) request(ctx context.Context, method, path string, form url.Values, authenticated bool) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, failure("protocol", "Invalid API path.")
	}
	if authenticated && c.token == "" {
		return nil, failure("auth", "Run valiido auth login first.")
	}
	authRetried := false
	for attempt := 0; ; attempt++ {
		if authenticated && !c.expiresAt.IsZero() && c.expiresAt.Before(time.Now().Add(time.Minute)) {
			if err := c.refreshAccess(ctx); err != nil {
				return nil, err
			}
		}
		// Rebuild the request each attempt; the form body reader is consumed by Do.
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
		if err != nil {
			return nil, failure("protocol", "Could not build API request.")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "valiido-cli/"+version)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, failure("cancelled", "Request cancelled or pairing expired.")
			}
			// The original error can include URLs or server-controlled content.
			return nil, failure("network", "Could not reach the Valiido server.")
		}
		if authenticated && resp.StatusCode == http.StatusUnauthorized && !authRetried && c.refresh != nil {
			resp.Body.Close()
			if err := c.refreshAccess(ctx); err != nil {
				return nil, err
			}
			authRetried = true
			attempt--
			continue
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRateLimitRetries {
			return resp, nil
		}
		// A 429 is returned by the rate-limit middleware before the request is
		// processed, so retrying (including writes) is safe. Wait, then retry.
		delay := retryAfterDelay(resp, attempt)
		resp.Body.Close()
		if c.retryLog != nil {
			fmt.Fprintf(c.retryLog, "Rate limited; retrying in %s (attempt %d/%d).\n", delay, attempt+1, maxRateLimitRetries)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, failure("cancelled", "Request cancelled or pairing expired.")
		case <-timer.C:
		}
	}
}

func (c *client) refreshAccess(ctx context.Context) error {
	if c.refresh == nil {
		return failure("auth", "Authentication expired. Run valiido auth login again.")
	}
	token, expiresAt, err := c.refresh(ctx, c.token)
	if err != nil {
		return err
	}
	c.token = token
	c.expiresAt = expiresAt
	return nil
}

// retryAfterDelay honours a numeric Retry-After header when present, otherwise
// backs off exponentially (1,2,4,… capped at 30s) to let the rate window clear.
func retryAfterDelay(resp *http.Response, attempt int) time.Duration {
	const maxDelay = 30 * time.Second
	if header := strings.TrimSpace(resp.Header.Get("Retry-After")); header != "" {
		if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
			delay := time.Duration(seconds) * time.Second
			if delay > maxDelay {
				delay = maxDelay
			}
			return delay
		}
	}
	if retryBackoffBase <= 0 {
		return 0
	}
	delay := retryBackoffBase << uint(attempt)
	if delay > maxDelay || delay <= 0 {
		delay = maxDelay
	}
	return delay
}

func (c *client) json(ctx context.Context, method, path string, form url.Values, authenticated, oauth bool, out any) error {
	resp, err := c.request(ctx, method, path, form, authenticated)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		return failure("protocol", "API response could not be read or exceeded the size limit.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if oauth && resp.StatusCode == http.StatusBadRequest {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &e) == nil {
				switch e.Error {
				case "authorization_pending", "slow_down", "access_denied", "expired_token", "invalid_grant":
					return failure(e.Error, "Authorization has not completed or is no longer valid.")
				}
			}
		}
		return responseError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return failure("protocol", "Server returned an invalid JSON response.")
	}
	return nil
}

func responseError(status int, data []byte) error {
	var payload struct {
		Error *apiError      `json:"error"`
		Data  map[string]any `json:"data"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != nil &&
		payload.Error.Code != "" && payload.Error.Message != "" {
		payload.Error.Details = payload.Data
		return payload.Error
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return failure("invalid_input", "The server rejected the request input.")
	case http.StatusUnauthorized:
		return failure("auth", "Authorization expired or was revoked. Run valiido auth login.")
	case http.StatusForbidden:
		return failure("forbidden", "This connection cannot access this resource.")
	case http.StatusNotFound:
		return failure("not_found", "Resource or API endpoint not found.")
	case http.StatusConflict:
		return failure("conflict", "The resource changed or the request conflicts with its current state.")
	case http.StatusTooManyRequests:
		return failure("rate_limited", "Rate limit reached. Try again later.")
	default:
		return failure("api", fmt.Sprintf("API request failed (HTTP %d).", status))
	}
}

func (c *client) examples(ctx context.Context, kind, query string, limit int) (examplePage, error) {
	if limit < 1 || limit > pageSize {
		return examplePage{}, failure("usage", "Example page size must be between 1 and 50.")
	}
	all := make([]example, 0)
	cursor := ""
	seen := map[string]bool{}
	ids := map[int64]bool{}
	for {
		input, _ := json.Marshal(map[string]any{"query": query, "limit": limit, "cursor": cursor})
		result, err := c.runCommand(ctx, kind+".list_examples", string(input), false)
		if err != nil {
			return examplePage{}, err
		}
		var page examplePage
		if err := decodeCommandData(result, &page); err != nil {
			return examplePage{}, err
		}
		if page.Type != kind || page.Examples == nil || len(page.Examples) > limit || page.Complete != (page.NextCursor == "") {
			return examplePage{}, failure("protocol", "Example list is missing from API response.")
		}
		for _, item := range page.Examples {
			if item.ExampleID <= 0 || item.Label == "" || item.Attributes == nil || ids[item.ExampleID] {
				return examplePage{}, failure("protocol", "API returned an invalid or duplicate example.")
			}
			ids[item.ExampleID] = true
			all = append(all, item)
			if len(all) > maxListItems {
				return examplePage{}, failure("protocol", "Example list exceeded the safety limit.")
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			return examplePage{Type: kind, Examples: all, Complete: true}, nil
		}
		if seen[cursor] || len(cursor) > maxCursorBytes {
			return examplePage{}, failure("protocol", "Example pagination did not make progress or exceeded the safety limit.")
		}
		seen[cursor] = true
	}
}

func (c *client) me(ctx context.Context) (identity, error) {
	var me identity
	err := c.json(ctx, "GET", apiPaths["api/identities#show"], nil, true, false, &me)
	if err == nil && (!tenantPattern.MatchString(me.TenantNumber) || !validAgentScope(me.Scope)) {
		err = failure("protocol", "Server did not return a regular tenant with the expected access.")
	}
	return me, err
}

func validAgentScope(scope string) bool {
	return scope == agentScope || scope == "isms:read" || scope == legacyReadScope || scope == legacyFullScope
}

func (c *client) resources(ctx context.Context, kind, query string) ([]resource, error) {
	if strings.TrimSpace(kind) == "" {
		return nil, failure("usage", "Resource type is required.")
	}
	all := make([]resource, 0)
	cursor := ""
	seen := map[string]bool{}
	ids := map[int64]bool{}
	for {
		input, _ := json.Marshal(map[string]any{"limit": pageSize, "cursor": cursor, "query": query})
		var page resourcePage
		result, err := c.runCommand(ctx, kind+".list", string(input), false)
		if err != nil {
			return nil, err
		}
		if err := decodeCommandData(result, &page); err != nil {
			return nil, err
		}
		if page.Type != kind || page.Resources == nil || len(page.Resources) > pageSize || page.Complete != (page.NextCursor == "") {
			return nil, failure("protocol", "Resource list is missing from API response.")
		}
		for _, item := range page.Resources {
			if err := validateResource(item, kind); err != nil {
				return nil, err
			}
			if ids[item.ID] {
				return nil, failure("protocol", "API returned a duplicate resource across pages.")
			}
			ids[item.ID] = true
			all = append(all, item)
			if len(all) > maxListItems {
				return nil, failure("protocol", "Resource list exceeded the safety limit.")
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			return all, nil
		}
		if seen[cursor] || len(cursor) > maxCursorBytes {
			return nil, failure("protocol", "API pagination did not make progress or exceeded the safety limit.")
		}
		seen[cursor] = true
	}
}

func (c *client) readResource(ctx context.Context, kind string, id int64) (resource, error) {
	if strings.TrimSpace(kind) == "" {
		return resource{}, failure("usage", "Resource type is required.")
	}
	var item resource
	result, err := c.runCommand(ctx, kind+".show", fmt.Sprintf(`{"id":%d}`, id), false)
	if err == nil {
		err = decodeCommandData(result, &item)
	}
	if err == nil {
		err = validateResource(item, kind)
	}
	if err == nil && item.ID != id {
		err = failure("protocol", "Server returned a different resource.")
	}
	return item, err
}

func validateResource(item resource, kind string) error {
	if item.ID <= 0 || item.Version == nil || *item.Version < 0 || item.Type != kind || item.Attributes == nil {
		return failure("protocol", "API returned an invalid resource.")
	}
	return nil
}

func (c *client) commands(ctx context.Context, query string, limit int) (commandCatalog, error) {
	var catalog commandCatalog
	params := url.Values{}
	if query != "" {
		params.Set("query", query)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	path := apiPaths["api/commands#index"]
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	err := c.json(ctx, "GET", path, nil, true, false, &catalog)
	if err == nil && catalog.Commands == nil {
		err = failure("protocol", "Command catalog is missing from API response.")
	}
	return catalog, err
}

func (c *client) runCommand(ctx context.Context, name, input string, dryRun bool) (commandResult, error) {
	var result commandResult
	form := url.Values{"name": {name}, "input": {input}}
	if dryRun {
		form.Set("dry_run", "true")
	}
	err := c.json(ctx, "POST", apiPaths["api/commands#create"], form, true, false, &result)
	if err == nil {
		err = validateCommandResult(result)
	}
	return result, err
}

func decodeCommandData(result commandResult, out any) error {
	data, err := json.Marshal(result.Data)
	if err != nil || json.Unmarshal(data, out) != nil {
		return failure("protocol", "Command returned invalid resource data.")
	}
	return nil
}

func (c *client) resolveCommand(ctx context.Context, action, token string) (commandResult, error) {
	var result commandResult
	err := c.json(ctx, "POST", apiPaths["api/commands#"+action], url.Values{"confirmation_token": {token}}, true, false, &result)
	if err == nil {
		err = validateCommandResult(result)
	}
	return result, err
}

func validateCommandResult(result commandResult) error {
	if !result.OK {
		if result.Error != nil && result.Error.Code != "" && result.Error.Message != "" {
			return result.Error
		}
		return failure("protocol", "Command result is missing from API response.")
	}
	if result.Error != nil || result.Summary == "" {
		return failure("protocol", "Command result is missing from API response.")
	}
	return nil
}

func (c *client) documents(ctx context.Context) ([]document, error) {
	items, err := c.resources(ctx, "documents", "")
	if err != nil {
		return nil, err
	}
	documents := make([]document, 0, len(items))
	for _, item := range items {
		d, err := documentFromResource(item)
		if err != nil {
			return nil, err
		}
		documents = append(documents, d)
	}
	return documents, nil
}

func validateDocument(d document) error {
	if d.ID <= 0 || (d.Kind != "rich_text" && d.Kind != "pdf" && d.Kind != "folder") {
		return failure("protocol", "API returned an invalid document.")
	}
	return nil
}

func (c *client) read(ctx context.Context, id int64) (document, error) {
	item, err := c.readResource(ctx, "documents", id)
	if err != nil {
		return document{}, err
	}
	d, err := documentFromResource(item)
	if err == nil && d.ID != id {
		err = failure("protocol", "Server returned a different document.")
	}
	return d, err
}

func documentFromResource(item resource) (document, error) {
	d := document{ID: item.ID, Version: *item.Version}
	var ok bool
	d.Title, ok = item.Attributes["title"].(string)
	if !ok {
		return document{}, failure("protocol", "Document title is missing from API response.")
	}
	d.Kind, _ = item.Attributes["kind"].(string)
	d.UpdatedAt, _ = item.Attributes["updated_at"].(string)
	d.ContentText, _ = item.Attributes["content_text"].(string)
	d.ContentState, _ = item.Attributes["content_state"].(string)
	if value, present := item.Attributes["parent_id"].(float64); present && value > 0 {
		parent := int64(value)
		d.ParentID = &parent
	}
	if err := validateDocument(d); err != nil {
		return document{}, err
	}
	return d, nil
}

func (c *client) pair(ctx context.Context, instructions io.Writer) (tokenResponse, error) {
	var device deviceResponse
	err := c.json(ctx, "POST", apiPaths["oauth#device_authorization"], url.Values{
		"client_id": {clientID}, "scope": {agentScope}, "agent_name": {localAgentName()},
	}, false, true, &device)
	if err != nil {
		return tokenResponse{}, err
	}
	u, err := url.Parse(device.VerificationURI)
	base, _ := url.Parse(c.base)
	if err != nil || len(device.VerificationURI) > maxPairingURI || len(device.VerificationURIComplete) > maxPairingURI ||
		u.User != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.Fragment != "" || !validOpaqueToken(device.DeviceCode) ||
		!regexp.MustCompile(`^[A-Z0-9-]{6,16}$`).MatchString(device.UserCode) || device.ExpiresIn < 1 || device.ExpiresIn > maxPairingSeconds {
		return tokenResponse{}, failure("protocol", "Server returned invalid pairing instructions.")
	}
	openURI := u
	if device.VerificationURIComplete != "" {
		complete, completeErr := url.Parse(device.VerificationURIComplete)
		if completeErr != nil || complete.User != nil || complete.Scheme != base.Scheme || complete.Host != base.Host || complete.Fragment != "" {
			return tokenResponse{}, failure("protocol", "Server returned invalid pairing instructions.")
		}
		openURI = complete
	}
	interval := device.Interval
	if interval == 0 {
		interval = 5
	}
	if interval < 1 || interval > 60 {
		return tokenResponse{}, failure("protocol", "Invalid pairing interval.")
	}
	if _, err := fmt.Fprintf(instructions, "Open %s\nConfirm code: %s\nOnly approve the connection you just started.\n", openURI.String(), device.UserCode); err != nil {
		return tokenResponse{}, failure("output", "Could not write pairing instructions.")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	for {
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tokenResponse{}, failure("cancelled", "Pairing cancelled or expired.")
		case <-timer.C:
		}
		var token tokenResponse
		err := c.json(ctx, "POST", apiPaths["oauth#token"], url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":  {clientID}, "device_code": {device.DeviceCode},
		}, false, true, &token)
		if err == nil {
			return token, validateToken(token, agentScope)
		}
		var ae *apiError
		if errors.As(err, &ae) {
			if ae.Code == "authorization_pending" {
				continue
			}
			if ae.Code == "slow_down" {
				interval += 5
				continue
			}
		}
		return tokenResponse{}, err
	}
}

func localAgentName() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "Valiido CLI"
	}
	return strings.TrimSpace(hostname)
}

func validateToken(token tokenResponse, expectedScope string) error {
	if !validOpaqueToken(token.AccessToken) || !validOpaqueToken(token.RefreshToken) || !strings.EqualFold(token.TokenType, "Bearer") ||
		!validAgentScope(token.Scope) || (expectedScope != "" && token.Scope != expectedScope) ||
		token.ExpiresIn < 1 || token.ExpiresIn > maxAccessSeconds {
		return failure("protocol", "Server returned an invalid or over-scoped token.")
	}
	return nil
}

func validOpaqueToken(value string) bool {
	return len(value) > 0 && len(value) <= maxTokenBytes && opaqueTokenPattern.MatchString(value)
}
