package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

func parseCommandInput(commandInput, commandInputFile string, commandInputProvided bool) (string, error) {
	if commandInputProvided && commandInputFile != "" {
		return "", failure("usage", "Use either --input or --input-file, not both.")
	}
	if commandInputFile != "" {
		var err error
		commandInput, err = readCommandInputFile(commandInputFile)
		if err != nil {
			return "", err
		}
	}
	if len(commandInput) > maxCommandInput {
		return "", failure("usage", "Command input cannot exceed 1 MiB.")
	}
	inputOption := "--input"
	if commandInputFile != "" {
		inputOption = "--input-file"
	}
	var value any
	if err := json.Unmarshal([]byte(commandInput), &value); err != nil {
		return "", failure("usage", inputOption+" must contain valid JSON.")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return "", failure("usage", inputOption+" must be a JSON object.")
	}
	return commandInput, nil
}

func listCommands(call *invocation) error {
	options, err := call.parseListOptions(1000, false)
	if err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		catalog, err := client.commands(ctx, options.query, options.limit)
		if err != nil {
			return err
		}
		if !catalog.Complete {
			return moreResults(call.output, catalog, "command list", options.query)
		}
		return success(call.output, catalog)
	})
}

func invokeCommand(call *invocation) error {
	name, err := call.takeArgument("Expected a command name before options.")
	if err != nil {
		return err
	}
	if !commandNamePattern.MatchString(name) {
		return usageError("Command name must have the form resource.action and cannot exceed 129 characters.")
	}
	var input, filename string
	var dryRun bool
	options := listOptions{pageSize: pageSize}
	flags, err := call.parse(func(flags *flag.FlagSet) {
		flags.StringVar(&input, "input", "{}", "Command input as a JSON object")
		flags.StringVar(&filename, "input-file", "", "Read command input from a JSON file")
		flags.BoolVar(&dryRun, "dry-run", false, "Validate and preview without changing data")
		flags.IntVar(&options.limit, "limit", 0, "Maximum number of results")
		flags.StringVar(&options.cursor, "cursor", "", "Resume from a previous list response")
	}, func() error {
		if options.limit < 0 || options.limit > maxListItems {
			return usageError(fmt.Sprintf("--limit must be between 1 and %d.", maxListItems))
		}
		if len(options.cursor) > maxCursorBytes {
			return usageError("--cursor exceeds the maximum length.")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if optionProvided(flags, "limit") && options.limit == 0 {
		return usageError(fmt.Sprintf("--limit must be between 1 and %d.", maxListItems))
	}
	input, err = parseCommandInput(input, filename, optionProvided(flags, "input"))
	if err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		if options.limit > 0 || options.cursor != "" {
			return pageCommand(ctx, client, call.output, name, input, options)
		}
		result, err := client.runCommand(ctx, name, input, dryRun)
		if err != nil {
			return err
		}
		if name == "comments.list" {
			moreComments(call.output, &result, input)
		}
		return commandSuccess(call.output, result)
	})
}

// Special lists page by number, opaque cursor or ID. --cursor sets the
// field the schema names and --limit follows pages until it has N rows. Lists
// that cannot resume inside a page return whole pages.
var pagingFields = []string{"cursor", "page", "before", "before_id"}

func pageCommand(ctx context.Context, c *client, out *output, name, input string, options listOptions) error {
	schema, found, err := c.commandSchema(ctx, name)
	if err != nil || !found {
		if err == nil {
			// Rails owns unknown-command and authorization errors.
			_, err = c.runCommand(ctx, name, input, false)
		}
		if err == nil {
			err = usageError("This action does not page. Check its input schema with command list.")
		}
		return err
	}
	field := ""
	for _, key := range pagingFields {
		if schema[key] != nil {
			field = key
			break
		}
	}
	limitSpec, hasLimit := schema["limit"].(map[string]any)
	if field == "" && (options.cursor != "" || !hasLimit) {
		return usageError("This action does not page. Check its input schema with command list.")
	}
	var values map[string]any
	_ = decodeJSON([]byte(input), &values)
	if options.cursor != "" {
		if values[field], err = pagingValue(field, options.cursor); err != nil {
			return err
		}
	}
	var rows []any
	var data map[string]any
	key, next := "", ""
	seen := map[string]bool{options.cursor: true}
	for {
		if hasLimit && options.limit > 0 {
			number, _ := limitSpec["maximum"].(json.Number)
			maximum, err := number.Int64()
			if err != nil || maximum < 1 {
				maximum = int64(options.limit)
			}
			values["limit"] = min(int64(options.limit-len(rows)), maximum)
		}
		payload, _ := json.Marshal(values)
		result, err := c.runCommand(ctx, name, string(payload), false)
		if err != nil {
			return err
		}
		data = result.Data
		key = listKey(data)
		if key == "" {
			return failure("protocol", "List is missing from API response.")
		}
		page, _ := data[key].([]any)
		rows = append(rows, page...)
		next = nextToken(data)
		if next == "" || field == "" || options.limit == 0 || len(rows) >= options.limit {
			break
		}
		if seen[next] || len(seen) > maxListItems {
			return failure("protocol", "List pagination did not make progress or exceeded the safety limit.")
		}
		seen[next] = true
		if values[field], err = pagingValue(field, next); err != nil {
			return failure("protocol", "List returned an invalid continuation.")
		}
	}
	delete(data, "next_page")
	delete(data, "next_before_id")
	delete(data, "next_cursor")
	data[key], data["count"], data["complete"] = rows, len(rows), next == ""
	if next != "" {
		data["next_cursor"] = next
	}
	delete(values, field)
	delete(values, "limit")
	noun, verb, _ := strings.Cut(name, ".")
	path := displayNoun(noun) + " " + displayVerb(verb)
	if len(values) > 0 {
		filters, _ := json.Marshal(values)
		path += " --input " + shellArg(string(filters))
	}
	return listSuccess(out, data, path, next, options)
}

func pagingValue(field, cursor string) (any, error) {
	if field == "cursor" {
		return cursor, nil
	}
	return parseResourceID(cursor)
}

func listKey(data map[string]any) string {
	keys := make([]string, 0, len(data))
	for key, value := range data {
		if _, ok := value.([]any); ok {
			keys = append(keys, key)
		}
	}
	if len(keys) != 1 {
		return ""
	}
	return keys[0]
}

func nextToken(data map[string]any) string {
	for _, key := range []string{"next_cursor", "next_page", "next_before_id"} {
		switch value := data[key].(type) {
		case string:
			return value
		case json.Number:
			return value.String()
		}
	}
	return ""
}

func moreComments(out *output, result *commandResult, input string) {
	cursor, _ := result.Data["next_cursor"].(json.Number)
	if number, err := cursor.Int64(); err != nil || number <= 0 {
		return
	}
	var scope struct {
		Type string      `json:"resource_type"`
		ID   json.Number `json:"resource_id"`
	}
	if json.Unmarshal([]byte(input), &scope) != nil || !resourceTypePattern.MatchString(scope.Type) {
		return
	}
	id, err := scope.ID.Int64()
	if err != nil || id <= 0 {
		return
	}
	next, _ := json.Marshal(map[string]any{"resource_type": scope.Type, "resource_id": id, "before": cursor})
	result.Breadcrumbs = append(result.Breadcrumbs, breadcrumb{Action: "load_more",
		Command:     "valiido comments list --input " + shellArg(string(next)) + profileOption(out.profile),
		Description: "Load older comments"})
}

func confirmCommand(call *invocation) error { return call.resolve("confirm") }
func rejectCommand(call *invocation) error  { return call.resolve("reject") }

func (call *invocation) resolve(action string) error {
	token, err := call.takeArgument("Expected a confirmation token or - for standard input.")
	if err != nil {
		return err
	}
	if _, err := call.parse(nil, nil); err != nil {
		return err
	}
	if token == "-" {
		if terminal(os.Stdin) {
			return usageError("Pipe the confirmation token to standard input.")
		}
		token, err = readConfirmation(call.context, os.Stdin)
		if err != nil {
			return err
		}
	}
	if token == "" || len(token) > 4096 || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return usageError("Pass one confirmation token of at most 4096 bytes without whitespace.")
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		result, err := client.resolveCommand(ctx, action, token)
		if err != nil {
			return err
		}
		return commandSuccess(call.output, result)
	})
}

func readConfirmation(ctx context.Context, input io.Reader) (string, error) {
	line := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 1024), maxJSON+4096)
		if scanner.Scan() {
			line <- strings.TrimSpace(scanner.Text())
		} else {
			line <- ""
		}
	}()
	select {
	case <-ctx.Done():
		return "", failure("cancelled", "Reading the confirmation token was cancelled.")
	case token := <-line:
		switch {
		case strings.HasPrefix(token, "\""):
			var decoded string
			if json.Unmarshal([]byte(token), &decoded) != nil {
				return "", usageError("Standard input must contain a confirmation token or a JSON string.")
			}
			token = decoded
		case strings.HasPrefix(token, "{"):
			var result struct {
				Token string `json:"confirmation_token"`
				OK    *bool  `json:"ok"`
				Data  *struct {
					Token string `json:"confirmation_token"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(token), &result) != nil {
				return "", usageError("Could not read the preparation result on standard input.")
			}
			token = result.Token
			if result.Data != nil {
				token = result.Data.Token
			}
			if token == "" || result.OK != nil && !*result.OK {
				return "", usageError("Preparation returned no single confirmation token.")
			}
		case strings.HasPrefix(token, "["):
			return "", usageError("Pass one confirmation token at a time.")
		}
		return token, nil
	}
}
