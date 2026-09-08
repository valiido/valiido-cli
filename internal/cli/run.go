package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const help = `Valiido CLI — ISMS access for people and agents

  valiido setup [--agent codex|claude|all] [--force]
  valiido auth login [--profile NAME] [--server URL] [--file-credentials]
  valiido auth status [--profile NAME]
  valiido auth logout [--profile NAME]
  valiido resource list TYPE [--profile NAME] [--json]
  valiido resource read TYPE ID [--profile NAME] [--json]
  valiido resource examples TYPE [--query TEXT] [--limit N] [--profile NAME] [--json]
  valiido resource pdf TYPE ID --output FILE.pdf [--profile NAME]
  valiido command list [--profile NAME] [--json]
  valiido command run NAME [--input JSON] [--dry-run] [--profile NAME]
  valiido command confirm TOKEN [--profile NAME]
  valiido command reject TOKEN [--profile NAME]
  valiido api schema [--profile NAME]

JSON is the default output. Resource list retrieves every page.
Use command list to discover resource types, actions and input schemas.
IDs are tenant-scoped. PDFs require --output; files are never overwritten.
Login shows a URL and code for browser approval. It does not open a browser.
Use --server http://127.0.0.1:3000 for local development.
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, help)
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, "valiido 0.1.0")
		return 0
	}
	err := run(ctx, args, stdout, stderr)
	if err == nil {
		return 0
	}
	ae := &apiError{Code: "error", Message: "Command failed."}
	var typed *apiError
	if errors.As(err, &typed) {
		ae = typed
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{"ok": false, "error": ae})
	switch ae.Code {
	case "usage":
		return 2
	case "auth", "invalid_grant", "access_denied", "expired_token":
		return 3
	case "forbidden":
		return 4
	case "not_found":
		return 5
	default:
		return 1
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	args = append([]string(nil), args...)
	args[0] = canonicalNoun(args[0])

	if args[0] == "setup" {
		return setup(args[1:], stdout)
	}
	if len(args) < 2 {
		return failure("usage", "Expected a command. Run valiido --help.")
	}
	command := args[0] + " " + args[1]
	rest := args[2:]
	var id int64
	var resourceType string
	if command == "documents read" || command == "tasks read" || command == "risks read" {
		if len(rest) == 0 {
			return failure("usage", "Expected a positive resource ID before options.")
		}
		var err error
		id, err = strconv.ParseInt(rest[0], 10, 64)
		if err != nil || id <= 0 {
			return failure("usage", "Resource ID must be a positive integer.")
		}
		rest = rest[1:]
	}
	if command == "resources list" || command == "resources examples" {
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a resource type before options.")
		}
		resourceType, rest = rest[0], rest[1:]
	}
	if command == "resources read" {
		if len(rest) < 2 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a resource type and positive resource ID before options.")
		}
		resourceType = rest[0]
		var err error
		id, err = strconv.ParseInt(rest[1], 10, 64)
		if err != nil || id <= 0 {
			return failure("usage", "Resource ID must be a positive integer.")
		}
		rest = rest[2:]
	}
	if command == "resources pdf" {
		if len(rest) < 2 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a resource type and positive resource ID before options.")
		}
		resourceType = rest[0]
		var err error
		id, err = strconv.ParseInt(rest[1], 10, 64)
		if err != nil || id <= 0 {
			return failure("usage", "Resource ID must be a positive integer.")
		}
		rest = rest[2:]
	}
	var commandName, confirmationToken string
	if command == "commands run" {
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a command name before options.")
		}
		commandName, rest = rest[0], rest[1:]
	}
	if command == "commands confirm" || command == "commands reject" {
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a confirmation token before options.")
		}
		confirmationToken, rest = rest[0], rest[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "default", "Credential profile")
	jsonOutput := flags.Bool("json", false, "JSON output (default)")
	var server, output, commandInput, query string
	var limit int
	var fileCredentials, text, dryRun bool
	switch command {
	case "auth login":
		flags.StringVar(&server, "server", "https://app.valiido.com", "Server origin")
		flags.BoolVar(&fileCredentials, "file-credentials", false, "Explicitly allow mode-0600 credential file")
	case "documents read":
		flags.BoolVar(&text, "text", false, "Plain text output")
		flags.StringVar(&output, "output", "", "PDF output filename")
	case "resources pdf":
		flags.StringVar(&output, "output", "", "PDF output filename")
	case "resources examples":
		flags.StringVar(&query, "query", "", "Search example names and content")
		flags.IntVar(&limit, "limit", 20, "Maximum number of examples (1-50)")
	case "commands run":
		flags.StringVar(&commandInput, "input", "{}", "Command input as a JSON object")
		flags.BoolVar(&dryRun, "dry-run", false, "Validate and preview without changing data")
	case "auth status", "auth logout", "documents list", "tasks list", "tasks read", "risks list", "risks read", "resources list", "resources read", "commands list", "commands confirm", "commands reject", "api schema":
	default:
		return failure("usage", "Unknown command. Run valiido --help.")
	}
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, help)
			return nil
		}
		return failure("usage", "Invalid options. Run valiido --help.")
	}
	if flags.NArg() != 0 || (text && (output != "" || *jsonOutput)) || (command == "resources pdf" && output == "") {
		return failure("usage", "Unexpected arguments or incompatible output options.")
	}
	if command == "resources examples" && (limit < 1 || limit > 50) {
		return failure("usage", "--limit must be between 1 and 50.")
	}
	s, unlock, err := openStore(*profile, fileCredentials)
	if err != nil {
		return err
	}
	defer unlock()
	if command == "auth login" {
		c, err := newClient(server, "")
		if err != nil {
			return err
		}
		if fileCredentials {
			fmt.Fprintln(stderr, "Credentials will be stored in a private mode-0600 file on this machine.")
		}
		tokens, err := c.pair(ctx, stderr)
		if err != nil {
			return err
		}
		c.token = tokens.AccessToken
		me, err := c.me(ctx)
		if err != nil {
			_ = revoke(ctx, c, tokens.RefreshToken)
			return err
		}
		if me.Scope != tokens.Scope {
			_ = revoke(ctx, c, tokens.RefreshToken)
			return failure("protocol", "Connection scope changed. Refusing the request.")
		}
		value := session{Server: c.base, TenantNumber: me.TenantNumber}
		applyTokens(&value, tokens)
		if err := s.save(value); err != nil {
			_ = revoke(ctx, c, tokens.RefreshToken)
			return err
		}
		return success(stdout, me)
	}
	value, err := s.load()
	if err != nil {
		return err
	}
	c, err := newClient(value.Server, value.AccessToken)
	if err != nil {
		return err
	}
	if command == "auth logout" {
		if err := revoke(ctx, c, value.RefreshToken); err != nil {
			return err // Keep credentials so a failed server-side revocation can be retried.
		}
		if err := s.clear(); err != nil {
			return err
		}
		return success(stdout, map[string]bool{"revoked": true})
	}
	if value.ExpiresAt.Before(time.Now().Add(30 * time.Second)) {
		var tokens tokenResponse
		err := c.json(ctx, "POST", apiPaths["oauth#token"], url.Values{"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {value.RefreshToken}}, false, true, &tokens)
		if err != nil {
			return err // Never blindly retry refresh token rotation.
		}
		if err := validateToken(tokens, value.Scope); err != nil {
			return err
		}
		applyTokens(&value, tokens)
		if err := s.save(value); err != nil {
			_ = revoke(ctx, c, tokens.RefreshToken)
			return err
		}
		c.token = tokens.AccessToken
	}
	me, err := c.me(ctx)
	if err != nil {
		return err
	}
	if me.TenantNumber != value.TenantNumber {
		return failure("protocol", "Connection tenant changed. Refusing the request.")
	}
	if value.Scope != "" && me.Scope != value.Scope {
		return failure("protocol", "Connection scope changed. Refusing the request.")
	}
	if value.Scope == "" {
		value.Scope = me.Scope
		if err := s.save(value); err != nil {
			return err
		}
	}
	switch command {
	case "auth status":
		return success(stdout, me)
	case "documents list":
		documents, err := c.documents(ctx)
		if err != nil {
			return err
		}
		return success(stdout, map[string]any{"documents": documents, "count": len(documents), "complete": true})
	case "documents read":
		d, err := c.read(ctx, id)
		if err != nil {
			return err
		}
		if output != "" {
			if d.Kind != "pdf" {
				return failure("usage", "--output is only for PDF documents; use --text for text documents.")
			}
			if err := c.download(ctx, "documents", id, output); err != nil {
				return err
			}
			return success(stdout, map[string]any{"id": id, "saved_to": output})
		}
		if text {
			if d.Kind != "rich_text" || d.ContentState != "text" {
				return failure("usage", "Document has no text body. For PDFs, use --output FILE.pdf.")
			}
			_, err := fmt.Fprintln(stdout, strings.Map(func(r rune) rune {
				if (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) && r != '\n' && r != '\t' {
					return -1
				}
				return r
			}, d.ContentText))
			return err
		}
		return success(stdout, d)
	case "tasks list", "risks list":
		kind := strings.Fields(command)[0]
		items, err := c.resources(ctx, kind)
		if err != nil {
			return err
		}
		return success(stdout, map[string]any{kind: items, "count": len(items), "complete": true})
	case "tasks read", "risks read":
		kind := strings.Fields(command)[0]
		item, err := c.readResource(ctx, kind, id)
		if err != nil {
			return err
		}
		return success(stdout, item)
	case "resources list":
		items, err := c.resources(ctx, resourceType)
		if err != nil {
			return err
		}
		return success(stdout, map[string]any{"type": resourceType, "resources": items, "count": len(items), "complete": true})
	case "resources read":
		item, err := c.readResource(ctx, resourceType, id)
		if err != nil {
			return err
		}
		return success(stdout, item)
	case "resources examples":
		input, err := json.Marshal(map[string]any{"query": query, "limit": limit})
		if err != nil {
			return failure("usage", "Could not encode example search options.")
		}
		result, err := c.runCommand(ctx, resourceType+".list_examples", string(input), false)
		if err != nil {
			return err
		}
		return success(stdout, result.Data)
	case "resources pdf":
		if err := c.download(ctx, resourceType, id, output); err != nil {
			return err
		}
		return success(stdout, map[string]any{"type": resourceType, "id": id, "saved_to": output})
	case "commands list":
		catalog, err := c.commands(ctx)
		if err != nil {
			return err
		}
		return success(stdout, catalog)
	case "commands run":
		var input map[string]any
		if len(commandInput) > 64<<10 || json.Unmarshal([]byte(commandInput), &input) != nil || input == nil {
			return failure("usage", "--input must be a JSON object no larger than 64 KiB.")
		}
		result, err := c.runCommand(ctx, commandName, commandInput, dryRun)
		if err != nil {
			return err
		}
		return success(stdout, result)
	case "commands confirm", "commands reject":
		action := strings.Fields(command)[1]
		result, err := c.resolveCommand(ctx, action, confirmationToken)
		if err != nil {
			return err
		}
		return success(stdout, result)
	case "api schema":
		var schema map[string]any
		if err := c.json(ctx, "GET", apiPaths["api/documentation#show"], nil, true, false, &schema); err != nil {
			return err
		}
		return success(stdout, schema)
	}
	return nil
}

func canonicalNoun(noun string) string {
	switch noun {
	case "document":
		return "documents"
	case "task":
		return "tasks"
	case "risk":
		return "risks"
	case "resource":
		return "resources"
	case "command":
		return "commands"
	default:
		return noun
	}
}

func applyTokens(value *session, tokens tokenResponse) {
	value.AccessToken = tokens.AccessToken
	value.RefreshToken = tokens.RefreshToken
	value.Scope = tokens.Scope
	value.ExpiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
}

func revoke(ctx context.Context, c *client, token string) error {
	return c.json(ctx, "POST", apiPaths["oauth#revoke"], url.Values{"client_id": {clientID}, "token": {token}}, false, true, nil)
}

func success(out io.Writer, data any) error {
	return json.NewEncoder(out).Encode(map[string]any{"ok": true, "data": data})
}

func (c *client) download(ctx context.Context, resourceType string, id int64, output string) error {
	resp, err := c.request(ctx, "GET", fmt.Sprintf(apiPaths["api/resources#pdf"], url.PathEscape(resourceType), id), nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/pdf") {
		return failure("api", "PDF download failed or returned a non-PDF response.")
	}
	// Validate the header before creating a file. Never follow a signed blob URL.
	header := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, header); err != nil || string(header) != "%PDF-" {
		return failure("protocol", "Response does not have a PDF signature.")
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return failure("storage", "Cannot create output file; existing files are never overwritten.")
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(output)
		}
	}()
	const maxPDF = 50 << 20
	written, err := io.Copy(f, io.LimitReader(io.MultiReader(strings.NewReader(string(header)), resp.Body), maxPDF+1))
	if err != nil || written > maxPDF {
		return failure("protocol", "PDF download was incomplete or exceeded 50 MiB.")
	}
	if err := f.Close(); err != nil {
		return failure("storage", "Could not finish saving PDF.")
	}
	ok = true
	return nil
}
