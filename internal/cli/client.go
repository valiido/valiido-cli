// Package cli implements the Valiido CLI.
package cli

import (
	"bytes"
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
	pageSize          = 250
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
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
	status  int
}

func (e *apiError) Error() string { return e.Message }

func failure(code, message string) *apiError { return &apiError{Code: code, Message: message} }

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
	// The ActionText HTML that an update must preserve.
	Content string `json:"content,omitempty"`
}

type resource struct {
	Type       string         `json:"type"`
	ID         int64          `json:"id"`
	Version    *int64         `json:"version,omitempty"`
	Label      string         `json:"label"`
	Attributes map[string]any `json:"attributes"`
	URL        string         `json:"url,omitempty"`
}

type resourcePage struct {
	Type       string     `json:"type"`
	Resources  []resource `json:"resources"`
	Count      int        `json:"count"`
	NextCursor string     `json:"next_cursor,omitempty"`
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
	Count      int       `json:"count"`
	NextCursor string    `json:"next_cursor"`
	Complete   bool      `json:"complete"`
}

type commandCatalog struct {
	Commands   []map[string]any `json:"commands"`
	Count      int              `json:"count"`
	TotalCount int              `json:"total_count"`
	Complete   bool             `json:"complete"`
}

type commandResult struct {
	OK          bool           `json:"ok"`
	Data        map[string]any `json:"data"`
	Summary     string         `json:"summary"`
	Breadcrumbs []breadcrumb   `json:"breadcrumbs"`
	Warnings    []string       `json:"warnings"`
	DryRun      bool           `json:"dry_run,omitempty"`
	Error       *apiError      `json:"error,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	receivedAt   time.Time
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
	profile       string
	session       *session
	verifiedViaMe bool // One /me lookup covers headerless responses for this invocation.
	base          string
	token         string
	refreshAt     time.Time
	refresh       func(context.Context, bool) (string, time.Time, error)
	retryLog      io.Writer
	http          *http.Client
}

func newClient(server, token string) (*client, error) {
	if len(server) > 2048 {
		return nil, failure("usage", "Server origin cannot exceed 2048 bytes.")
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" ||
		u.RawPath != "" || (u.Path != "" && u.Path != "/") {
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

// Rails permits refresh during the last two minutes of access-token validity.
const tokenRefreshWindow = 2 * time.Minute

func (c *client) requestAPI(ctx context.Context, method, path string, form url.Values, authenticated bool, bufferSuccess func(*http.Response) (*http.Response, error)) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, failure("protocol", "Invalid API path.")
	}
	if authenticated && c.token == "" && c.refresh == nil {
		return nil, failure("auth", "Run valiido auth login first.")
	}
	authRetries := 0
	const maxAuthRetries = 3
	checkedExpiry := false
	policy := apiRetryPolicy(method, path)
	return c.withRetry(ctx, policy, c.http.Timeout, func() (*http.Response, error) {
		for {
			if authenticated && !checkedExpiry && c.refresh != nil && (c.token == "" ||
				!c.refreshAt.IsZero() && !c.refreshAt.After(time.Now())) {
				checkedExpiry = true
				if err := c.refreshAccess(ctx, c.token == ""); err != nil {
					return nil, err
				}
			}
			resp, err := c.sendAPIRequest(ctx, method, path, form, authenticated)
			if err != nil {
				return nil, err
			}
			if authenticated && resp.StatusCode == http.StatusUnauthorized && authRetries < maxAuthRetries && c.refresh != nil {
				checkedExpiry = true
				resp.Body.Close()
				previous := c.token
				if err := c.refreshAccess(ctx, true); err != nil {
					return nil, err
				}
				authRetries++
				if c.token == previous {
					authRetries = maxAuthRetries
				}
				continue
			}
			if bufferSuccess == nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				return resp, nil
			}
			if resp.StatusCode >= 300 {
				return bufferResponse(resp, maxJSON)
			}
			return bufferSuccess(resp)
		}
	})
}

func (c *client) sendAPIRequest(ctx context.Context, method, path string, form url.Values, authenticated bool) (*http.Response, error) {
	if authenticated && c.token == "" {
		return nil, failure("auth", "Run valiido auth login first.")
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
	return c.do(c.http, req)
}

func (c *client) do(httpClient *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := httpClient.Do(req)
	if err == nil && c.session != nil && req.Header.Get("Authorization") != "" {
		if err := c.checkResponseIdentity(req.Context(), req.URL.Path, resp); err != nil {
			resp.Body.Close()
			return nil, err
		}
	}
	return resp, err
}

func (c *client) refreshAccess(ctx context.Context, forced bool) error {
	token, refreshAt, err := c.refresh(ctx, forced)
	if err != nil {
		return err
	}
	c.token = token
	c.refreshAt = refreshAt
	return nil
}

func (c *client) json(ctx context.Context, method, path string, form url.Values, authenticated, oauth bool, out any) error {
	resp, err := c.requestAPI(ctx, method, path, form, authenticated, func(resp *http.Response) (*http.Response, error) {
		return bufferResponse(resp, maxJSON)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || oauth && resp.StatusCode == http.StatusTooManyRequests {
		return responseError(resp.StatusCode, nil)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		if resp.StatusCode == 429 {
			return responseError(resp.StatusCode, nil)
		}
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
	if err := decodeJSON(data, out); err != nil {
		return failure("protocol", "Server returned an invalid JSON response.")
	}
	if tokens, ok := out.(*tokenResponse); ok {
		tokens.receivedAt = time.Now()
	}
	return nil
}

func decodeJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return failure("protocol", "Unexpected data after the JSON response.")
	}
	return nil
}

func responseError(status int, data []byte) error {
	if transientStatus(status) {
		err := failure("server_unavailable", fmt.Sprintf("Server returned HTTP %d.", status))
		err.status = status
		return err
	}

	var payload struct {
		Error *apiError      `json:"error"`
		Data  map[string]any `json:"data"`
	}
	if decodeJSON(data, &payload) == nil && payload.Error != nil &&
		payload.Error.Code != "" && payload.Error.Message != "" {
		payload.Error.Hint = ""
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

type listOptions struct {
	query, cursor   string
	limit, pageSize int
}

func (o listOptions) resultLimit() int {
	if o.limit > 0 {
		return o.limit
	}
	return maxListItems
}

func (c *client) examples(ctx context.Context, kind string, options listOptions) (examplePage, error) {
	all := make([]example, 0)
	cursor := options.cursor
	seen := map[string]bool{cursor: true}
	ids := map[int64]bool{}
	for {
		limit := min(options.pageSize, options.resultLimit()-len(all))
		input, _ := json.Marshal(map[string]any{"query": options.query, "limit": limit, "cursor": cursor})
		result, err := c.runCommand(ctx, kind+".list_examples", string(input), false)
		if err != nil {
			return examplePage{}, resourceCommandError(kind, err)
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
		}
		cursor = page.NextCursor
		if cursor != "" && (seen[cursor] || len(cursor) > maxCursorBytes) {
			return examplePage{}, failure("protocol", "Example pagination did not make progress or exceeded the safety limit.")
		}
		if page.Complete || len(all) == options.resultLimit() {
			return examplePage{Type: kind, Examples: all, Count: len(all), NextCursor: cursor, Complete: page.Complete}, nil
		}
		seen[cursor] = true
	}
}

func (c *client) me(ctx context.Context) (identity, error) {
	var me identity
	err := c.json(ctx, "GET", apiPaths["api/identities#show"], nil, true, false, &me)
	if err != nil {
		return identity{}, err
	}
	if err := c.checkIdentity(ctx, me); err != nil {
		return identity{}, err
	}
	c.verifiedViaMe = true
	return me, nil
}

func validAgentScope(scope string) bool {
	return scope == agentScope || scope == "isms:read" || scope == legacyReadScope || scope == legacyFullScope
}

func (c *client) resources(ctx context.Context, kind string, options listOptions) (resourcePage, error) {
	if strings.TrimSpace(kind) == "" {
		return resourcePage{}, failure("usage", "Resource type is required.")
	}
	all := make([]resource, 0)
	cursor := options.cursor
	seen := map[string]bool{cursor: true}
	ids := map[int64]bool{}
	for {
		limit := min(options.pageSize, options.resultLimit()-len(all))
		input := map[string]any{"limit": limit, "cursor": cursor}
		if kind != "guide_items" || options.query != "" {
			input["query"] = options.query
		}
		payload, _ := json.Marshal(input)
		var page resourcePage
		result, err := c.runCommand(ctx, kind+".list", string(payload), false)
		if err != nil {
			return resourcePage{}, resourceCommandError(kind, err)
		}
		if err := decodeCommandData(result, &page); err != nil {
			return resourcePage{}, err
		}
		if page.Type != kind || page.Resources == nil || len(page.Resources) > limit || page.Complete != (page.NextCursor == "") {
			return resourcePage{}, failure("protocol", "Resource list is missing from API response.")
		}
		for _, item := range page.Resources {
			if kind == "guide_items" {
				if item.ID <= 0 || item.Type != kind || item.Attributes == nil {
					return resourcePage{}, failure("protocol", "API returned an invalid guide item.")
				}
			} else if err := validateResource(item, kind); err != nil {
				return resourcePage{}, err
			}
			if ids[item.ID] {
				return resourcePage{}, failure("protocol", "API returned a duplicate resource across pages.")
			}
			ids[item.ID] = true
			all = append(all, item)
		}
		cursor = page.NextCursor
		if cursor != "" && (seen[cursor] || len(cursor) > maxCursorBytes) {
			return resourcePage{}, failure("protocol", "API pagination did not make progress or exceeded the safety limit.")
		}
		if page.Complete || len(all) == options.resultLimit() {
			return resourcePage{Type: kind, Resources: all, Count: len(all), NextCursor: cursor, Complete: page.Complete}, nil
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
	err = resourceCommandError(kind, err)
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
	// Read-only resources such as full audits have no version.
	if item.ID <= 0 || item.Version != nil && *item.Version < 0 || item.Type != kind || item.Attributes == nil {
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
	if err == nil && (catalog.Commands == nil || catalog.Count != len(catalog.Commands) || (limit > 0 && catalog.Count > limit) || catalog.TotalCount < catalog.Count || catalog.Complete != (catalog.Count == catalog.TotalCount)) {
		err = failure("protocol", "Command catalog is missing from API response.")
	}
	return catalog, err
}

func resourceCommandError(kind string, err error) error {
	if err == nil || strings.HasSuffix(kind, "s") {
		return err
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Code == "unknown_command" {
		return failure(apiErr.Code, apiErr.Message+" Resource types are plural; try "+kind+"s.")
	}
	return err
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
	if err != nil || decodeJSON(data, out) != nil {
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
	return result, uncertainWrite(err, "The change may already have been processed. Check its state with resource read or resource list before preparing another change.")
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
	page, err := c.resources(ctx, "documents", listOptions{pageSize: pageSize})
	if err != nil {
		return nil, err
	}
	return documentsFromResources(page.Resources)
}

func documentsFromResources(items []resource) ([]document, error) {
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

func documentFromResource(item resource) (document, error) {
	if item.Version == nil {
		return document{}, failure("protocol", "Document version is missing from API response.")
	}
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
	d.Content, _ = item.Attributes["content"].(string)
	if value, _ := item.Attributes["parent_id"].(json.Number); value != "" {
		parent, err := value.Int64()
		if err != nil || parent <= 0 {
			return document{}, failure("protocol", "Server returned an invalid parent ID.")
		}
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
