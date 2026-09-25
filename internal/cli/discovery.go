package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
)

var helpTopics = map[string]string{
	"output": `Output
  Terminal     Tables and compact lists
  Pipe         JSON envelope
  --json       Force the JSON envelope
  --quiet      JSON data only
  --jq EXPR    Filter the envelope with jq from PATH
  --ids-only   One ID per line
  --count      Result count
  --markdown   Markdown tables and lists
  --styled     Force human-readable output

Terminal errors go to stderr. Piped invocations return a JSON error with a
stable code, a hint and a recovery breadcrumb when available.
IDs and counts refer to the returned page or batch. The retried field counts
automatic replays. Use NO_COLOR to turn off terminal styling.`,
	"exit-codes": `Exit codes
  0  Success
  1  Other failure, including network failure and cancellation
  2  Usage
  3  Authentication
  4  Authorization
  5  Missing resource
  6  Invalid input
  7  Temporary server failure, rate limit or busy profile
  8  Local storage
  9  Conflict`,
	"environment": `Environment
  PATH             Keyring utilities and optional jq
  HOME             Agent skill installation and configuration fallback
  XDG_CONFIG_HOME  Configuration directory on Linux and other Unix systems
  APPDATA          Configuration directory on Windows
  NO_COLOR         Disable terminal color and emphasis
  TERM             Use dumb to disable terminal emphasis
  HTTPS_PROXY      Proxy for HTTPS requests
  HTTP_PROXY       Proxy for HTTP requests
  NO_PROXY         Proxy exclusions

Select credentials with --profile NAME.`,
}

// The noun and action stay in the server namespace. Rails validates them
// against the same registry used to build the live command catalog.
func resourceShortcut(args []string) []string {
	if len(args) < 2 {
		return args
	}
	noun, depth := resolveNoun(args)
	if len(args) <= depth {
		return args
	}
	args = append([]string{noun, canonicalVerb(args[depth])}, args[depth+1:]...)
	noun = args[0]
	if !resourceTypePattern.MatchString(noun) {
		return args
	}
	if reservedNoun(noun) {
		return args
	}
	verb := args[1]
	switch verb {
	case "list", "show":
		if noun == "comments" || noun == "references" || hasCommandInput(args[2:]) {
			break
		}
		fallthrough
	case "examples", "import", "pdf":
		legacy := canonicalNoun(noun) + " " + verb
		if _, ok := commandsByName[legacy]; ok {
			return args
		}
		return append([]string{"resource", verb, noun}, args[2:]...)
	case "upload":
		if _, ok := commandsByName[canonicalNoun(noun)+" "+verb]; ok {
			return args
		}
	}
	name := canonicalNoun(noun) + "." + verb
	if !commandNamePattern.MatchString(name) {
		return args
	}
	return append([]string{"command", "run", name}, positionalID(args[2:])...)
}

// "valiido tasks show 42" reads like the rest of the CLI; the ID becomes the command input.
func positionalID(args []string) []string {
	if len(args) == 0 || !positiveIDPattern.MatchString(args[0]) || slices.ContainsFunc(args[1:], isInputOption) {
		return args
	}
	return append([]string{"--input", `{"id":` + args[0] + `}`}, args[1:]...)
}

func isInputOption(arg string) bool {
	name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	return strings.HasPrefix(arg, "-") && (name == "input" || name == "input-file")
}

func reservedNoun(noun string) bool {
	group, ok := findCommandGroup(canonicalNoun(noun))
	return ok && (group.section != "" || group.display == "help")
}

func hasCommandInput(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(arg, "=")
		switch strings.TrimLeft(name, "-") {
		case "input", "input-file", "dry-run":
			return true
		}
	}
	return false
}

// Conventional resources use the paginated resource contract. A special
// command with the same verb keeps the input and result shape in its schema.
func (c *client) commandSchema(ctx context.Context, name string) (map[string]any, bool, error) {
	catalog, err := c.commands(ctx, name, 0)
	if err != nil {
		return nil, false, err
	}
	for _, entry := range catalog.Commands {
		if entry["name"] == name {
			schema, _ := entry["input_schema"].(map[string]any)
			return schema, true, nil
		}
	}
	return nil, false, nil
}

func (c *client) specialResourceRead(ctx context.Context, name string, id int64, options listOptions) (commandResult, bool, error) {
	schema, found, err := c.commandSchema(ctx, name)
	if err != nil {
		return commandResult{}, false, err
	}
	if found {
		if id == 0 && schema["query"] != nil && schema["cursor"] != nil {
			return commandResult{}, false, nil
		}
		idSpec, _ := schema["id"].(map[string]any)
		if id > 0 && len(schema) == 1 && idSpec["required"] == true && idSpec["description"] == nil {
			return commandResult{}, false, nil
		}
		input := map[string]any{}
		if options.cursor != "" {
			if name != "chats.list" {
				return commandResult{}, true, failure("usage", "This action does not accept --cursor.")
			}
			before, err := parseResourceID(options.cursor)
			if err != nil {
				return commandResult{}, true, err
			}
			input["before_id"] = before
		}
		// The extra entry lets the CLI report whether a server-limited feed has more.
		if spec, ok := schema["limit"].(map[string]any); ok && options.limit > 0 {
			number, _ := spec["maximum"].(json.Number)
			if maximum, err := number.Int64(); err == nil && maximum >= 1 {
				input["limit"] = min(options.limit+1, int(maximum))
			}
		}
		if id == 0 && options.pageSize != pageSize {
			return commandResult{}, true, usageError("This list uses the server's page size.")
		}
		if id > 0 {
			input["id"] = id
		}
		if options.query != "" {
			if schema["query"] == nil {
				return commandResult{}, true, failure("usage", "This action does not accept --query. Check its input schema with command list.")
			}
			input["query"] = options.query
		}
		data, _ := json.Marshal(input)
		result, err := c.runCommand(ctx, name, string(data), false)
		return result, true, err
	}
	// Send unlisted names to Rails so it owns unknown-command and authorization errors.
	return commandResult{}, false, nil
}

func specialListSuccess(w *output, result commandResult, options listOptions) error {
	if options.limit == 0 && options.cursor == "" && result.Data["next_before_id"] == nil {
		return commandSuccess(w, result)
	}
	for _, key := range []string{"resources", "access_levels", "chats"} {
		rows, ok := result.Data[key].([]any)
		if !ok {
			continue
		}
		count := min(len(rows), options.resultLimit())
		complete := count == len(rows) && result.Data["next_before_id"] == nil
		result.Data[key] = rows[:count]
		result.Data["count"] = count
		result.Data["complete"] = complete
		if !complete {
			path := displayNoun(w.kind) + " list"
			if key == "chats" {
				number, _ := result.Data["next_before_id"].(json.Number)
				if count < len(rows) {
					last, _ := rows[count-1].(map[string]any)
					number, _ = last["id"].(json.Number)
				}
				id, err := number.Int64()
				if err != nil || id <= 0 {
					return failure("protocol", "Chat list is missing a continuation ID.")
				}
				cursor := strconv.FormatInt(id, 10)
				result.Data["next_cursor"] = cursor
				return listSuccess(w, result.Data, path, cursor, options)
			}
			if w.kind == "changelog" {
				path += " --limit 50"
			}
			return moreResults(w, result.Data, path, options.query)
		}
		return success(w, result.Data)
	}
	return failure("protocol", "List is missing from API response.")
}

func doctor(ctx context.Context, w *output, profile string, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, commandRequestBudget)
	defer cancel()
	checks := map[string]any{"profile": profile, "version": version, "lock": "unknown", "login": "unchecked", "server": "unchecked"}
	diagnose := func(err error) error {
		var typed *apiError
		if errors.As(err, &typed) {
			copy := *typed
			copy.Details = checks
			return &copy
		}
		return err
	}
	s, unlock, err := openStoreContext(ctx, profile, false, 0)
	if err != nil {
		var typed *apiError
		if errors.As(err, &typed) && typed.Code == "busy" {
			checks["lock"] = "busy"
		}
		return diagnose(err)
	}
	checks["lock"] = "available"
	value, err := s.load()
	unlock()
	if err != nil {
		checks["login"] = "unavailable"
		return diagnose(err)
	}
	checks["login"] = "stored"
	c, err := newClient(value.Server, value.AccessToken)
	if err != nil {
		return diagnose(err)
	}
	checks["origin"] = c.base
	c.retryLog = stderr
	c.useSession(profile, value)
	_, err = c.me(ctx)
	if err != nil {
		var typed *apiError
		if errors.As(err, &typed) && typed.Code == "auth" {
			checks["server"] = "reachable"
			checks["login"] = "refresh required"
		} else {
			checks["server"] = "check failed"
		}
		return diagnose(err)
	}
	checks["server"] = "reachable"
	checks["login"] = "active"
	if c.token != value.AccessToken {
		checks["login"] = "refreshed"
	}
	return writeResponse(w, response{OK: true, Summary: "Profile diagnostics", Data: checks})
}

func runDoctor(call *invocation) error {
	if _, err := call.parse(nil, nil); err != nil {
		return err
	}
	return doctor(call.context, call.output, call.output.profile, call.stderr)
}

func showSchema(call *invocation) error {
	if _, err := call.parse(nil, nil); err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		var schema map[string]any
		if err := client.json(ctx, "GET", apiPaths["api/documentation#show"], nil, true, false, &schema); err != nil {
			return err
		}
		return success(call.output, schema)
	})
}
