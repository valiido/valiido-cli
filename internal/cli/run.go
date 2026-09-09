package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var resourceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var commandNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}\.[a-z][a-z0-9_]{0,63}$`)

type commandDefinition struct {
	name  string
	usage string
}

var commandDefinitions = []commandDefinition{
	{"help", "valiido --help"},
	{"version", "valiido --version"},
	{"setup", "valiido setup [--agent codex|claude|all] [--force]"},
	{"auth login", "valiido auth login [--profile NAME] [--server URL] [--file-credentials]"},
	{"auth status", "valiido auth status [--profile NAME]"},
	{"auth logout", "valiido auth logout [--profile NAME]"},
	{"documents list", "valiido document list [--profile NAME] [--json]"},
	{"documents read", "valiido document read ID [--text | --output FILE.pdf] [--profile NAME] [--json]"},
	{"documents upload", "valiido document upload FILE [--profile NAME]"},
	{"tasks list", "valiido task list [--profile NAME] [--json]"},
	{"tasks read", "valiido task read ID [--profile NAME] [--json]"},
	{"risks list", "valiido risk list [--profile NAME] [--json]"},
	{"risks read", "valiido risk read ID [--profile NAME] [--json]"},
	{"resources list", "valiido resource list TYPE [--query TEXT] [--profile NAME] [--json]"},
	{"resources read", "valiido resource read TYPE ID [--profile NAME] [--json]"},
	{"resources examples", "valiido resource examples TYPE [--query TEXT] [--limit N] [--profile NAME] [--json]"},
	{"resources import", "valiido resource import TYPE --file FILE.json [--dry-run] [--profile NAME]"},
	{"resources pdf", "valiido resource pdf TYPE ID --output FILE.pdf [--profile NAME]"},
	{"commands list", "valiido command list [--query TEXT] [--limit N] [--profile NAME] [--json]"},
	{"commands run", "valiido command run NAME [--input JSON | --input-file FILE.json] [--dry-run] [--profile NAME]"},
	{"commands confirm", "valiido command confirm TOKEN [--profile NAME]"},
	{"commands reject", "valiido command reject TOKEN [--profile NAME]"},
	{"api schema", "valiido api schema [--profile NAME]"},
}

var knownCommands = func() map[string]bool {
	commands := make(map[string]bool, len(commandDefinitions))
	for _, definition := range commandDefinitions {
		commands[definition.name] = true
	}
	return commands
}()

func commandUsage(name string) string {
	for _, definition := range commandDefinitions {
		if definition.name == name {
			return definition.usage
		}
	}
	return "valiido --help"
}

var help = func() string {
	lines := make([]string, 0, len(commandDefinitions))
	for _, definition := range commandDefinitions {
		lines = append(lines, "  "+definition.usage)
	}
	return "Valiido CLI — ISMS access for people and agents\n\n" + strings.Join(lines, "\n") + `

JSON is the default output. Resource list retrieves up to 10,000 records and accepts --query.
Examples retrieves up to 10,000 records; --limit controls the page size.
Import accepts a JSON array, validates every batch, and returns confirmation tokens.
Use command list to discover every command; --limit optionally caps the result at 1,000.
IDs are tenant-scoped. PDFs require --output; files are never overwritten.
PDF upload returns a signed attachment identifier for a document command.
Login shows a URL and code for browser approval. It does not open a browser.
Use --server http://127.0.0.1:3000 with auth login for local development.
Import error indexes are 1-based.
`
}()

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, help)
		return 0
	}
	if contextual, ok := contextualHelp(args); ok {
		fmt.Fprint(stdout, contextual)
		return 0
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintln(stdout, "valiido "+version)
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
	return exitCode(ae.Code)
}

func exitCode(code string) int {
	switch code {
	case "usage":
		return 2
	case "unknown_command", "invalid_input", "validation_failed", "unsupported_resource_attributes":
		return 6
	case "auth", "invalid_grant", "access_denied", "expired_token":
		return 3
	case "forbidden", "not_authorized":
		return 4
	case "not_found":
		return 5
	case "rate_limited", "slow_down", "too_many_pending":
		return 7
	case "storage":
		return 8
	case "conflict":
		return 9
	default:
		return 1
	}
}

func contextualHelp(args []string) (string, bool) {
	if len(args) < 2 || (args[len(args)-1] != "--help" && args[len(args)-1] != "-h") {
		return "", false
	}
	parts := append([]string(nil), args[:len(args)-1]...)
	parts[0] = canonicalNoun(parts[0])
	if parts[0] == "setup" {
		return "Usage:\n  " + commandUsage("setup") + "\n", true
	}
	if len(parts) >= 2 {
		name := strings.Join(parts[:2], " ")
		if knownCommands[name] {
			return "Usage:\n  " + commandUsage(name) + "\n", true
		}
	}
	if len(parts) == 1 {
		prefix := parts[0] + " "
		lines := []string{}
		for _, definition := range commandDefinitions {
			if strings.HasPrefix(definition.name, prefix) {
				lines = append(lines, "  "+definition.usage)
			}
		}
		if len(lines) > 0 {
			return "Usage:\n" + strings.Join(lines, "\n") + "\n", true
		}
	}
	return "", false
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	args = append([]string(nil), args...)
	if strings.HasPrefix(args[0], "-") {
		return failure("usage", "Options must follow a command. Run valiido --help.")
	}
	args[0] = canonicalNoun(args[0])

	if args[0] == "setup" {
		return setup(args[1:], stdout)
	}
	if len(args) < 2 {
		return failure("usage", "Expected a command. Run valiido --help.")
	}
	command := args[0] + " " + args[1]
	if !knownCommands[command] {
		return failure("usage", "Unknown command. Run valiido --help.")
	}
	rest := args[2:]
	var id int64
	var resourceType string
	var uploadFile string
	if command == "documents read" || command == "tasks read" || command == "risks read" {
		if len(rest) == 0 {
			return failure("usage", "Expected a positive resource ID before options.")
		}
		var err error
		id, err = parseResourceID(rest[0])
		if err != nil {
			return err
		}
		rest = rest[1:]
	}
	if command == "documents upload" {
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a PDF filename before options.")
		}
		uploadFile, rest = rest[0], rest[1:]
	}
	if command == "resources list" || command == "resources examples" || command == "resources import" {
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a resource type before options.")
		}
		resourceType, rest = strings.ToLower(rest[0]), rest[1:]
	}
	if command == "resources read" {
		if len(rest) < 2 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a resource type and positive resource ID before options.")
		}
		resourceType = strings.ToLower(rest[0])
		var err error
		id, err = parseResourceID(rest[1])
		if err != nil {
			return err
		}
		rest = rest[2:]
	}
	if command == "resources pdf" {
		if len(rest) < 2 || strings.TrimSpace(rest[0]) == "" {
			return failure("usage", "Expected a resource type and positive resource ID before options.")
		}
		resourceType = strings.ToLower(rest[0])
		var err error
		id, err = parseResourceID(rest[1])
		if err != nil {
			return err
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
	if resourceType != "" && !resourceTypePattern.MatchString(resourceType) {
		return failure("usage", "Resource type must use lowercase letters, numbers and underscores and cannot exceed 64 characters.")
	}
	if commandName != "" && !commandNamePattern.MatchString(commandName) {
		return failure("usage", "Command name must have the form resource.action and cannot exceed 129 characters.")
	}
	if confirmationToken != "" && (len(confirmationToken) > 4096 || strings.IndexFunc(confirmationToken, unicode.IsSpace) >= 0) {
		return failure("usage", "Confirmation token cannot contain whitespace or exceed 4096 bytes.")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "default", "Credential profile")
	jsonOutput := flags.Bool("json", false, "JSON output (default)")
	var server, output, commandInput, commandInputFile, query, importFile string
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
		flags.IntVar(&limit, "limit", pageSize, "Examples per page (1-250); every page is retrieved")
	case "resources list":
		flags.StringVar(&query, "query", "", "Search resource fields")
	case "resources import":
		flags.StringVar(&importFile, "file", "", "JSON array to import")
		flags.BoolVar(&dryRun, "dry-run", false, "Validate without preparing changes")
	case "commands list":
		flags.StringVar(&query, "query", "", "Search command names and descriptions")
		flags.IntVar(&limit, "limit", 0, "Maximum number of commands (1-1000)")
	case "commands run":
		flags.StringVar(&commandInput, "input", "{}", "Command input as a JSON object")
		flags.StringVar(&commandInputFile, "input-file", "", "Read command input from a JSON file")
		flags.BoolVar(&dryRun, "dry-run", false, "Validate and preview without changing data")
	case "auth status", "auth logout", "documents list", "documents upload", "tasks list", "tasks read", "risks list", "risks read", "resources read", "commands confirm", "commands reject", "api schema":
	default:
		return failure("usage", "Unknown command. Run valiido --help.")
	}
	if err := rejectDuplicateFlags(rest, flags); err != nil {
		return err
	}
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, help)
			return nil
		}
		return failure("usage", fmt.Sprintf("Invalid options. Usage: %s", commandUsage(command)))
	}
	limitProvided := false
	commandInputProvided := false
	flags.Visit(func(option *flag.Flag) {
		limitProvided = limitProvided || option.Name == "limit"
		commandInputProvided = commandInputProvided || option.Name == "input"
	})
	if flags.NArg() != 0 {
		return failure("usage", fmt.Sprintf("Unexpected positional arguments. Usage: %s", commandUsage(command)))
	}
	if text && output != "" {
		return failure("usage", "Use either --text or --output, not both.")
	}
	if text && *jsonOutput {
		return failure("usage", "Use either --text or --json, not both.")
	}
	if command == "resources pdf" && output == "" {
		return failure("usage", "--output is required for resource pdf.")
	}
	if command == "resources examples" && (limit < 1 || limit > pageSize) {
		return failure("usage", "--limit must be between 1 and 250.")
	}
	if command == "commands list" && limitProvided && (limit < 1 || limit > 1000) {
		return failure("usage", "--limit must be between 1 and 1000.")
	}
	if len(query) > 1024 {
		return failure("usage", "--query cannot exceed 1024 bytes.")
	}
	if command == "resources import" && importFile == "" {
		return failure("usage", "--file is required for resource import.")
	}
	var parsedCommandInput map[string]any
	if command == "commands run" {
		if commandInputProvided && commandInputFile != "" {
			return failure("usage", "Use either --input or --input-file, not both.")
		}
		if commandInputFile != "" {
			var err error
			commandInput, err = readCommandInputFile(commandInputFile)
			if err != nil {
				return err
			}
		}
		if len(commandInput) > maxCommandInput {
			return failure("usage", "Command input cannot exceed 1 MiB.")
		}
		inputOption := "--input"
		if commandInputFile != "" {
			inputOption = "--input-file"
		}
		var value any
		if err := json.Unmarshal([]byte(commandInput), &value); err != nil {
			return failure("usage", inputOption+" must contain valid JSON.")
		}
		var ok bool
		parsedCommandInput, ok = value.(map[string]any)
		if !ok || parsedCommandInput == nil {
			return failure("usage", inputOption+" must be a JSON object.")
		}
	}
	var importBatches [][]byte
	var importTotal int
	if command == "resources import" {
		var err error
		importBatches, importTotal, err = readImportBatches(importFile)
		if err != nil {
			return err
		}
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
		unlock()
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
		unlock()
		return success(stdout, map[string]bool{"revoked": true})
	}
	if value.ExpiresAt.Before(time.Now().Add(30 * time.Second)) {
		value, err = refreshSessionTokens(ctx, s, value)
		if err != nil {
			return err
		}
		c.token = value.AccessToken
	}
	c.retryLog = stderr
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
	// Access tokens can be used concurrently. Release the profile before the
	// actual command while keeping load, refresh rotation and saving serialized.
	unlock()
	c.expiresAt = value.ExpiresAt
	c.refresh = func(refreshContext context.Context, staleAccessToken string) (string, time.Time, error) {
		refreshed, refreshErr := refreshStoredSession(refreshContext, *profile, fileCredentials, staleAccessToken)
		if refreshErr != nil {
			return "", time.Time{}, refreshErr
		}
		return refreshed.AccessToken, refreshed.ExpiresAt, nil
	}
	switch command {
	case "auth status":
		return success(stdout, me)
	case "documents list":
		documents, err := c.documents(ctx)
		if err != nil {
			return err
		}
		return success(stdout, map[string]any{"type": "documents", "resources": documents, "count": len(documents), "complete": true})
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
	case "documents upload":
		upload, err := c.uploadFile(ctx, uploadFile)
		if err != nil {
			return err
		}
		return success(stdout, upload)
	case "tasks list", "risks list":
		kind := strings.Fields(command)[0]
		items, err := c.resources(ctx, kind, "")
		if err != nil {
			return err
		}
		return success(stdout, map[string]any{"type": kind, "resources": items, "count": len(items), "complete": true})
	case "tasks read", "risks read":
		kind := strings.Fields(command)[0]
		item, err := c.readResource(ctx, kind, id)
		if err != nil {
			return err
		}
		return success(stdout, item)
	case "resources list":
		items, err := c.resources(ctx, resourceType, query)
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
		result, err := c.examples(ctx, resourceType, query, limit)
		if err != nil {
			return err
		}
		return success(stdout, result)
	case "resources import":
		results, err := c.importResources(ctx, resourceType, importBatches, dryRun)
		if err != nil {
			return err
		}
		return success(stdout, map[string]any{
			"type": resourceType, "count": importTotal, "batches": results,
			"dry_run": dryRun, "confirmation_required": !dryRun,
		})
	case "resources pdf":
		if err := c.download(ctx, resourceType, id, output); err != nil {
			return err
		}
		return success(stdout, map[string]any{"type": resourceType, "id": id, "saved_to": output})
	case "commands list":
		catalog, err := c.commands(ctx, query, limit)
		if err != nil {
			return err
		}
		return success(stdout, catalog)
	case "commands run":
		result, err := c.runCommand(ctx, commandName, commandInput, dryRun)
		if err != nil {
			return err
		}
		return commandSuccess(stdout, result)
	case "commands confirm", "commands reject":
		action := strings.Fields(command)[1]
		result, err := c.resolveCommand(ctx, action, confirmationToken)
		if err != nil {
			return err
		}
		return commandSuccess(stdout, result)
	case "api schema":
		var schema map[string]any
		if err := c.json(ctx, "GET", apiPaths["api/documentation#show"], nil, true, false, &schema); err != nil {
			return err
		}
		return success(stdout, schema)
	}
	return nil
}

func refreshStoredSession(ctx context.Context, profile string, fileCredentials bool, staleAccessToken string) (session, error) {
	s, unlock, err := openStore(profile, fileCredentials)
	if err != nil {
		return session{}, err
	}
	defer unlock()

	value, err := s.load()
	if err != nil {
		return session{}, err
	}
	if value.AccessToken != staleAccessToken && value.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return value, nil
	}

	return refreshSessionTokens(ctx, s, value)
}

func refreshSessionTokens(ctx context.Context, s *store, value session) (session, error) {
	c, err := newClient(value.Server, "")
	if err != nil {
		return session{}, err
	}
	var tokens tokenResponse
	if err := c.json(ctx, "POST", apiPaths["oauth#token"], url.Values{
		"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {value.RefreshToken},
	}, false, true, &tokens); err != nil {
		return session{}, err
	}
	if err := validateToken(tokens, value.Scope); err != nil {
		return session{}, err
	}
	applyTokens(&value, tokens)
	if err := s.save(value); err != nil {
		_ = revoke(ctx, c, tokens.RefreshToken)
		return session{}, err
	}
	return value, nil
}

func rejectDuplicateFlags(args []string, flags *flag.FlagSet) error {
	seen := map[string]bool{}
	for index := 0; index < len(args); index++ {
		raw := args[index]
		if raw == "--" {
			break
		}
		if !strings.HasPrefix(raw, "-") || raw == "-" {
			continue
		}
		nameAndValue := strings.TrimLeft(raw, "-")
		name, _, hasValue := strings.Cut(nameAndValue, "=")
		option := flags.Lookup(name)
		if option == nil {
			continue
		}
		if seen[name] {
			return failure("usage", fmt.Sprintf("Option --%s may only be specified once.", name))
		}
		seen[name] = true
		if !hasValue {
			if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); !ok || !boolean.IsBoolFlag() {
				index++
			}
		}
	}
	return nil
}

func parseResourceID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, failure("usage", "Resource ID must be an integer from 1 to 9223372036854775807.")
	}
	return id, nil
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

func commandSuccess(out io.Writer, result commandResult) error {
	return json.NewEncoder(out).Encode(result)
}

func (c *client) download(ctx context.Context, resourceType string, id int64, output string) error {
	resp, err := c.request(ctx, "GET", fmt.Sprintf(apiPaths["api/resources#pdf"], url.PathEscape(resourceType), id), nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxJSON+1))
		if readErr != nil || len(data) > maxJSON {
			return failure("protocol", "API response could not be read or exceeded the size limit.")
		}
		return responseError(resp.StatusCode, data)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/pdf") {
		return failure("api", "PDF download failed or returned a non-PDF response.")
	}
	// Validate the header before creating a file. Never follow a signed blob URL.
	header := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, header); err != nil || string(header) != "%PDF-" {
		return failure("protocol", "Response does not have a PDF signature.")
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			return failure("storage", "Cannot create output file; existing files are never overwritten.")
		case errors.Is(err, os.ErrNotExist):
			return failure("storage", "Output directory does not exist: "+filepath.Dir(output))
		case errors.Is(err, os.ErrPermission):
			return failure("storage", "Permission denied while creating the output file.")
		default:
			return failure("storage", "Could not create the output file.")
		}
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
