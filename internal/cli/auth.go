package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

func (c *client) useSession(profile string, value session) {
	c.profile, c.session = profile, &value
	c.refreshAt = value.nextRefresh()
	c.refresh = func(ctx context.Context, forced bool) (string, time.Time, error) {
		refreshed, err := refreshStoredSession(ctx, profile, value, forced, c.retryLog)
		if err != nil {
			return "", time.Time{}, err
		}
		value = refreshed
		return value.AccessToken, value.nextRefresh(), nil
	}
}

func (c *client) checkResponseIdentity(ctx context.Context, path string, resp *http.Response) error {
	tenants, scopes := resp.Header.Values("Valiido-Tenant"), resp.Header.Values("Valiido-Scope")
	if len(tenants) == 0 && len(scopes) == 0 {
		if path == apiPaths["api/identities#show"] || resp.StatusCode == http.StatusUnauthorized || c.verifiedViaMe {
			return nil
		}
		// A legacy response must be verified with the same bearer that produced it.
		probe := *c
		probe.refresh = nil
		if _, err := probe.me(ctx); err != nil {
			return err
		}
		c.verifiedViaMe = true
		return nil
	}
	if len(tenants) != 1 || len(scopes) != 1 {
		return failure("protocol", "Server returned incomplete or ambiguous connection headers.")
	}
	return c.checkIdentity(ctx, identity{TenantNumber: tenants[0], Scope: scopes[0]})
}

func (c *client) checkIdentity(ctx context.Context, me identity) error {
	if !tenantPattern.MatchString(me.TenantNumber) || !validAgentScope(me.Scope) {
		return failure("protocol", "Server did not return a regular tenant with the expected access.")
	}
	value := c.session
	if value == nil {
		return nil
	}
	if value.TenantNumber != "" && me.TenantNumber != value.TenantNumber {
		return failure("protocol", "Connection tenant changed. Refusing the request.")
	}
	if value.Scope != "" && me.Scope != value.Scope {
		return failure("protocol", "Connection scope changed. Refusing the request.")
	}
	if value.Scope == "" && c.profile != "" {
		if err := rememberScope(ctx, c.profile, value, me.Scope); err != nil {
			return err
		}
	}
	value.TenantNumber, value.Scope = me.TenantNumber, me.Scope
	return nil
}

func login(ctx context.Context, s *store, server string, stderr io.Writer) (identity, error) {
	c, err := newClient(server, "")
	if err != nil {
		return identity{}, err
	}
	c.retryLog = stderr
	if s.file {
		fmt.Fprintln(stderr, "Credentials will be stored in a private mode-0600 file on this machine.")
	}
	tokens, err := c.pair(ctx, stderr)
	if err != nil {
		return identity{}, err
	}
	saved := false
	defer func() {
		if !saved {
			// Revoke even when cancellation caused the failure; the token was never stored.
			cleanup, cancel := cleanupContext(ctx)
			defer cancel()
			_ = revoke(cleanup, c, tokens.RefreshToken)
		}
	}()
	value := session{Server: c.base, Scope: tokens.Scope}
	c.session = &value
	c.token = tokens.AccessToken
	me, err := c.me(ctx)
	if err != nil {
		return identity{}, err
	}
	applyTokens(&value, tokens)
	if err := s.save(value); err != nil {
		return identity{}, err
	}
	saved = true
	return me, nil
}

func rememberScope(ctx context.Context, profile string, value *session, scope string) error {
	store, unlock, err := openStoreContext(ctx, profile, false, profileLockUntilDeadline)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := store.load()
	if err != nil {
		return err
	}
	if current.Server != value.Server || current.TenantNumber != value.TenantNumber ||
		current.AccessToken != value.AccessToken || current.RefreshToken != value.RefreshToken || current.Scope == scope {
		return nil
	}
	if current.Scope != "" {
		return failure("protocol", "Connection scope changed. Refusing the request.")
	}
	current.Scope = scope
	return store.save(current)
}

const commandRequestBudget = 120 * time.Second

// File transfers and multi-batch imports need more than one command budget.
const transferBudget = 10 * time.Minute

// transferRequestTimeout caps one upload or download request within transferBudget.
const transferRequestTimeout = 5 * time.Minute

const cleanupBudget = 15 * time.Second

// cleanupContext survives cancellation of ctx so best-effort cleanup still reaches the server.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
}

func (value session) nextRefresh() time.Time {
	if value.RefreshPending {
		return time.Time{}
	}
	// A server-side slow_down gives us a short interval in which to trust
	// the access token, even if the local expiry says it has already expired.
	due := value.ExpiresAt.Add(-tokenRefreshWindow)
	if value.RefreshAfter != nil && value.RefreshAfter.After(due) {
		return *value.RefreshAfter
	}
	return due
}

func refreshStoredSession(ctx context.Context, profile string, previous session, forced bool, retryLog io.Writer) (session, error) {
	s, unlock, err := openStoreContext(ctx, profile, false, profileLockUntilDeadline)
	if err != nil {
		return session{}, err
	}
	defer unlock()

	value, err := s.load()
	if err != nil {
		return session{}, err
	}
	if value.Server != previous.Server || value.TenantNumber != previous.TenantNumber ||
		(previous.Scope != "" && value.Scope != previous.Scope) {
		return session{}, failure("auth", "Profile changed while the command was running. Run the command again.")
	}
	if value.AccessToken != previous.AccessToken || value.RefreshToken != previous.RefreshToken {
		return value, nil
	}
	if !forced && value.AccessToken != "" &&
		(value.RefreshPending || value.nextRefresh().After(time.Now())) {
		return value, nil
	}
	return refreshSessionTokens(ctx, s, value, retryLog)
}

func refreshSessionTokens(ctx context.Context, s *store, value session, retryLog io.Writer) (session, error) {
	if value.RefreshPending {
		return session{}, failure("auth", "The previous token exchange could not be confirmed. Run valiido auth login again.")
	}
	if ctx.Err() != nil {
		return session{}, failure("cancelled", "Token renewal cancelled.")
	}
	c, err := newClient(value.Server, "")
	if err != nil {
		return session{}, err
	}
	c.retryLog = retryLog
	c.http.Timeout = commandRequestBudget
	// Persist the intent before sending: a killed process or lost response
	// must not cause another process to replay a potentially consumed token.
	value.RefreshPending = true
	if err := s.save(value); err != nil {
		return session{}, err
	}
	var tokens tokenResponse
	err = c.json(ctx, "POST", apiPaths["oauth#token"], url.Values{
		"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {value.RefreshToken},
	}, false, true, &tokens)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && (apiErr.Code == "slow_down" || apiErr.Code == "rate_limited" || apiErr.status == http.StatusServiceUnavailable) {
			// These responses reject the request before consuming the token.
			// 502 and 504 stay pending: an upstream may have processed the refresh.
			value.RefreshPending = false
			if apiErr.Code == "slow_down" {
				next := time.Now().Add(30 * time.Second)
				value.RefreshAfter = &next
			}
			if saveErr := s.save(value); saveErr != nil {
				return session{}, saveErr
			}
			if apiErr.Code == "slow_down" && value.AccessToken != "" {
				return value, nil
			}
		}
		return session{}, err
	}
	if err := validateToken(tokens, value.Scope); err != nil {
		return session{}, err
	}
	applyTokens(&value, tokens)
	if err := s.save(value); err != nil {
		return session{}, err
	}
	return value, nil
}

func applyTokens(value *session, tokens tokenResponse) {
	value.AccessToken = tokens.AccessToken
	value.RefreshToken = tokens.RefreshToken
	value.Scope = tokens.Scope
	value.ExpiresAt = tokens.receivedAt.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	value.RefreshPending = false
	value.RefreshAfter = nil
}

func revoke(ctx context.Context, c *client, token string) error {
	return c.json(ctx, "POST", apiPaths["oauth#revoke"], url.Values{"client_id": {clientID}, "token": {token}}, false, true, nil)
}

// Signing in again keeps the profile on its server; only a new profile defaults to production.
func profileServer(profile string) string {
	// Stale or rejected credentials still name their server: that is exactly when people sign in again.
	if s, err := profileStore(profile, false); err == nil {
		if value, _ := s.load(); value.Server != "" {
			return value.Server
		}
	}
	return "https://app.valiido.com"
}

func loginProfile(call *invocation) error {
	var server string
	var fileCredentials, dryRun bool
	if _, err := call.parse(func(flags *flag.FlagSet) {
		flags.StringVar(&server, "server", "", "Server origin")
		flags.BoolVar(&fileCredentials, "file-credentials", false, "Explicitly allow mode-0600 credential file")
		flags.BoolVar(&dryRun, "dry-run", false, "Show the login target without pairing")
	}, nil); err != nil {
		return err
	}
	if server == "" {
		server = profileServer(call.output.profile)
	}
	if dryRun {
		client, err := newClient(server, "")
		if err != nil {
			return err
		}
		storage := "keyring"
		if fileCredentials {
			storage = "file"
		}
		return writeResponse(call.output, response{OK: true, DryRun: true, Summary: "Login preview",
			Data: map[string]string{"profile": call.output.profile, "server": client.base, "credentials": storage}})
	}
	store, unlock, err := openStoreContext(call.context, call.output.profile, fileCredentials, profileLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	currentIdentity, err := login(call.context, store, server, call.stderr)
	if err != nil {
		return err
	}
	unlock()
	return writeResponse(call.output, response{OK: true, Summary: "Signed in", Data: currentIdentity})
}

func showProfile(call *invocation) error {
	if _, err := call.parse(nil, nil); err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		currentIdentity, err := client.me(ctx)
		if err != nil {
			return err
		}
		return writeResponse(call.output, response{OK: true, Summary: "Connected", Data: currentIdentity})
	})
}

func logoutProfile(call *invocation) error {
	var dryRun bool
	if _, err := call.parse(func(flags *flag.FlagSet) {
		flags.BoolVar(&dryRun, "dry-run", false, "Show the login to revoke")
	}, nil); err != nil {
		return err
	}
	if dryRun {
		store, err := profileStore(call.output.profile, false)
		if err != nil {
			return err
		}
		client, _, err := call.loadClient(store)
		if err != nil {
			return err
		}
		return writeResponse(call.output, response{OK: true, DryRun: true, Summary: "Logout preview",
			Data: map[string]string{"profile": call.output.profile, "server": client.base}})
	}
	ctx, cancel := context.WithTimeout(call.context, commandRequestBudget)
	defer cancel()
	store, unlock, err := openStoreContext(ctx, call.output.profile, false, profileLockUntilDeadline)
	if err != nil {
		return err
	}
	defer unlock()
	client, value, err := call.loadClient(store)
	if err != nil {
		return err
	}
	if err := revoke(ctx, client, value.RefreshToken); err != nil {
		return err // Keep credentials so a failed server-side revocation can be retried.
	}
	if err := store.clear(); err != nil {
		return err
	}
	unlock()
	return writeResponse(call.output, response{OK: true, Summary: "Signed out", Data: map[string]bool{"revoked": true}})
}

func (call *invocation) withSession(action func(context.Context, *client) error) error {
	return call.withSessionBudget(commandRequestBudget, action)
}

func (call *invocation) withSessionBudget(budget time.Duration, action func(context.Context, *client) error) error {
	ctx, cancel := context.WithTimeout(call.context, budget)
	defer cancel()
	// A busy profile is reported after the normal command budget, even for long transfers.
	lockCtx, cancelLock := context.WithTimeout(ctx, commandRequestBudget)
	defer cancelLock()
	store, unlock, err := openStoreContext(lockCtx, call.output.profile, false, profileLockUntilDeadline)
	if err != nil {
		return err
	}
	client, value, err := call.loadClient(store)
	unlock()
	if err != nil {
		return err
	}
	client.useSession(call.output.profile, value)
	call.output.origin, call.output.tenant = client.base, value.TenantNumber
	return action(ctx, client)
}

func (call *invocation) loadClient(store *store) (*client, session, error) {
	value, err := store.load()
	if err != nil {
		return nil, session{}, err
	}
	client, err := newClient(value.Server, value.AccessToken)
	if err != nil {
		return nil, session{}, err
	}
	client.retryLog = call.stderr
	client.http.Timeout = commandRequestBudget
	return client, value, nil
}
