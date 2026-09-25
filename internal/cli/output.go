package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type breadcrumb struct {
	Action      string `json:"action,omitempty"`
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`
	Label       string `json:"label,omitempty"`
	Path        string `json:"path,omitempty"`
}

type response struct {
	OK          bool         `json:"ok"`
	Data        any          `json:"data"`
	Summary     string       `json:"summary"`
	Breadcrumbs []breadcrumb `json:"breadcrumbs"`
	Retries     int          `json:"retried"`
	Warnings    *[]string    `json:"warnings,omitempty"`
	DryRun      bool         `json:"dry_run,omitempty"`
	URL         string       `json:"url,omitempty"`
}

type output struct {
	io.Writer
	ctx     context.Context
	retries *int
	profile string
	origin  string
	tenant  string
	kind    string
	mode    string
	filter  string
	jq      string
}

func outputFlags(flags *flag.FlagSet, writer *output) func() error {
	modes := []string{"json", "quiet", "ids-only", "count", "markdown", "styled"}
	for _, mode := range modes {
		flags.Bool(mode, false, "")
	}
	flags.StringVar(&writer.filter, "jq", "", "Filter JSON with jq")
	return func() error {
		writer.mode = ""
		if optionProvided(flags, "jq") {
			writer.mode = "jq"
		}
		for _, mode := range modes {
			if flags.Lookup(mode).Value.String() != "true" {
				continue
			}
			if writer.mode != "" {
				return usageError("Choose one output format. --" + writer.mode + " conflicts with --" + mode + ".")
			}
			writer.mode = mode
		}
		if writer.mode != "jq" {
			return nil
		}
		return writer.prepareFilter()
	}
}

func (writer *output) prepareFilter() error {
	if strings.TrimSpace(writer.filter) == "" {
		return usageError("--jq requires a filter.")
	}
	var err error
	writer.jq, err = exec.LookPath("jq")
	if err != nil {
		return usageError("--jq requires jq on PATH. Install jq or select --json.")
	}
	// An empty input stream compiles the filter without evaluating it.
	if err := writer.jqCommand().Run(); err != nil {
		if writer.ctx.Err() != nil {
			return failure("cancelled", "JSON filtering was cancelled.")
		}
		return usageError("jq could not compile the filter. Check the expression and quote it as a single argument.")
	}
	return nil
}

func (writer *output) jqCommand() *exec.Cmd {
	command := exec.CommandContext(writer.ctx, writer.jq, "--monochrome-output", "--", writer.filter)
	command.Env = []string{}
	return command
}

func success(out io.Writer, data any) error {
	return writeResponse(out, response{OK: true, Data: data})
}

func listSuccess(writer *output, data any, path, cursor string, options listOptions) error {
	if cursor == "" {
		return success(writer, data)
	}
	command := listCommand(writer, path, options.query) + " --cursor " + shellArg(cursor)
	if options.limit > 0 {
		command += " --limit " + strconv.Itoa(options.limit)
	}
	if options.pageSize != pageSize {
		command += " --page-size " + strconv.Itoa(options.pageSize)
	}
	return loadMore(writer, data, command, "Load more results")
}

func moreResults(writer *output, data any, path, query string) error {
	return loadMore(writer, data, listCommand(writer, path, query), "Load all available results")
}

func loadMore(writer *output, data any, command, description string) error {
	return writeResponse(writer, response{OK: true, Data: data, Breadcrumbs: []breadcrumb{{Action: "load_more", Command: command, Description: description}}})
}

func listCommand(writer *output, path, query string) string {
	command := "valiido " + path
	if query != "" {
		command += " --query " + shellArg(query)
	}
	if writer.profile != "" && writer.profile != "default" {
		command += " --profile " + shellArg(writer.profile)
	}
	return command
}

func shellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func commandSuccess(out io.Writer, result commandResult) error {
	return writeResponse(out, response{
		OK: true, Data: result.Data, Summary: result.Summary,
		Breadcrumbs: result.Breadcrumbs, Warnings: &result.Warnings, DryRun: result.DryRun,
	})
}

func writeResponse(out io.Writer, envelope response) error {
	writer, ok := out.(*output)
	if !ok {
		writer = &output{Writer: out}
	}
	if writer.retries != nil {
		envelope.Retries = *writer.retries
	}
	result, err := inspectResult(envelope.Data)
	if err != nil {
		return err
	}
	if envelope.Summary == "" {
		envelope.Summary = result.summary(writer.kind)
	}
	envelope.Breadcrumbs = append(envelope.Breadcrumbs, confirmationBreadcrumbs(result, writer.profile)...)
	kind := writer.kind
	if kind == "" {
		kind, _ = result.object["type"].(string)
	}
	if !envelope.DryRun {
		envelope.Breadcrumbs = append(result.readBreadcrumbs(kind, writer.profile), envelope.Breadcrumbs...)
	}
	if !result.listing && !envelope.DryRun {
		if envelope.URL == "" {
			raw, _ := result.object["url"].(string)
			envelope.URL = recordURL(writer.origin, writer.tenant, raw)
		}
		if envelope.URL != "" && result.recordID != "" && resourceTypePattern.MatchString(kind) {
			envelope.Breadcrumbs = append(envelope.Breadcrumbs, breadcrumb{Action: "open", Path: envelope.URL,
				Command: "valiido open " + displayNoun(kind) + " " + result.recordID + profileOption(writer.profile)})
		}
	}
	mode := writer.mode
	if mode == "" && terminal(writer.Writer) {
		mode = "styled"
	}
	switch mode {
	case "quiet":
		return json.NewEncoder(writer).Encode(envelope.Data)
	case "jq":
		return writer.filterResponse(envelope)
	case "ids-only":
		return result.writeIDs(writer)
	case "count":
		_, err := fmt.Fprintln(writer, result.count)
		return err
	case "markdown", "styled":
		return renderReadable(writer, envelope, result, kind, mode == "markdown")
	default:
		return json.NewEncoder(writer).Encode(envelope)
	}
}

func confirmationBreadcrumbs(result resultView, profile string) []breadcrumb {
	field := "the JSON preparation result"
	if result.object["confirmation_token"] == nil {
		if required, _ := result.object["confirmation_required"].(bool); !required || result.object["batches"] == nil {
			return nil
		}
		field = "one data.batches[].confirmation_token"
	}
	return []breadcrumb{
		{Action: "confirm", Command: "valiido command confirm -" + profileOption(profile), Description: "Pipe " + field + " to apply the change"},
		{Action: "reject", Command: "valiido command reject -" + profileOption(profile), Description: "Pipe " + field + " to discard the change"},
	}
}

func (writer *output) filterResponse(result response) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	command := writer.jqCommand()
	command.Stdin = bytes.NewReader(data)
	var filtered bytes.Buffer
	command.Stdout = &filtered
	if err := command.Run(); err != nil {
		if writer.ctx.Err() != nil {
			return failure("cancelled", "JSON filtering was cancelled.")
		}
		return usageError("jq could not evaluate the filter. Check the types and fields in the JSON envelope.")
	}
	_, err = filtered.WriteTo(writer)
	return err
}

func terminal(out io.Writer) bool {
	_, ok := writerTerminal(out)
	return ok
}

func (writer *output) readable() bool {
	return writer.mode == "styled" || writer.mode == "markdown" || writer.mode == "" && terminal(writer.Writer)
}

func writerTerminal(out io.Writer) (int, bool) {
	file, ok := out.(*os.File)
	if !ok {
		return 0, false
	}
	return terminalSize(file)
}

func profileOption(profile string) string {
	if profile == "" || profile == "default" {
		return ""
	}
	if !profilePattern.MatchString(profile) {
		return ""
	}
	return " --profile " + profile
}

type resultView struct {
	object   map[string]any
	rows     []resultRow
	count    int
	listing  bool
	recordID string
}

type resultRow struct {
	value any
	id    string
}

func inspectResult(data any) (resultView, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return resultView{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return resultView{}, err
	}
	object, single := value.(map[string]any)
	values, count, listing := resultRows(value, object)
	result := resultView{object: object, count: count, listing: !single || listing, recordID: rowID(value)}
	for _, value := range values {
		result.rows = append(result.rows, resultRow{value: value, id: rowID(value)})
	}
	return result, nil
}

func resultRows(value any, object map[string]any) ([]any, int, bool) {
	if rows, ok := value.([]any); ok {
		return rows, len(rows), true
	}
	for _, key := range []string{"resources", "examples", "commands", "batches", "access_levels", "chats", "skills"} {
		rows, ok := object[key].([]any)
		if !ok {
			continue
		}
		number, _ := object["count"].(json.Number)
		if count, err := strconv.Atoi(string(number)); err == nil {
			return rows, count, true
		}
		return rows, len(rows), true
	}
	if value == nil {
		return nil, 0, false
	}
	return []any{value}, 1, false
}

func (result resultView) writeIDs(writer io.Writer) error {
	for _, row := range result.rows {
		if row.id == "" {
			continue
		}
		if _, err := fmt.Fprintln(writer, row.id); err != nil {
			return err
		}
	}
	return nil
}

func (result resultView) summary(kind string) string {
	object := result.object
	if name, ok := object["type"].(string); ok {
		kind = name
	}
	kind = strings.ReplaceAll(kind, "_", " ")
	switch {
	case object["commands"] != nil:
		return fmt.Sprintf("%d commands", result.count)
	case object["examples"] != nil:
		return fmt.Sprintf("%d examples", result.count)
	case object["resources"] != nil && kind != "":
		return fmt.Sprintf("%d %s", result.count, kind)
	case object["batches"] != nil:
		batches, _ := object["batches"].([]any)
		return fmt.Sprintf("%d %s in %d batches", result.count, kind, len(batches))
	case object["saved_to"] != nil:
		return "PDF saved"
	case object["signed_id"] != nil:
		return "File uploaded"
	}
	if id := result.recordID; id != "" && kind != "" {
		return kind + " #" + id
	}
	if result.listing {
		return fmt.Sprintf("%d results", result.count)
	}
	return "Done"
}

func rowID(value any) string {
	obj, _ := value.(map[string]any)
	for _, key := range []string{"id", "example_id"} {
		id, _ := obj[key].(json.Number)
		if n, err := id.Int64(); err == nil && n > 0 {
			return string(id)
		}
	}
	return ""
}

func (result resultView) readBreadcrumbs(kind, profile string) []breadcrumb {
	crumbs := []breadcrumb{}
	if !validShortcut(kind, "show") {
		return crumbs
	}
	for _, row := range result.rows {
		obj, _ := row.value.(map[string]any)
		if row.id == "" || obj["example_id"] != nil {
			continue
		}

		command := "valiido " + displayNoun(kind) + " show " + row.id + profileOption(profile)
		crumbs = append(crumbs, breadcrumb{Action: "read", Command: command, Description: "Read " + kind + " #" + row.id})
	}
	return crumbs
}

func writeFailure(out *output, stderr io.Writer, failure *apiError, args []string, retries int) {
	err := *failure
	next, hint := recoverCommand(err.Code, args, out.profile)
	if err.Code == "auth" && err.Details["login"] == "unavailable" {
		next = "valiido auth login" + profileOption(out.profile)
		hint = "Sign in to the selected profile."
	}
	if err.Details["login"] == "identity mismatch" {
		next = "valiido auth login" + profileOption(out.profile)
	}
	if err.Hint == "" {
		err.Hint = hint
	}
	if strings.HasPrefix(err.Hint, "The change may already have been processed.") {
		next = "valiido resource --help"
	}
	if out.readable() {
		fmt.Fprintln(stderr, "Error  "+cleanDisplay(err.Message))
		for _, check := range []string{"profile", "lock", "login", "server"} {
			if value, ok := err.Details[check].(string); ok {
				fmt.Fprintln(stderr, check+"  "+cleanDisplay(value))
			}
		}
		fmt.Fprintln(stderr, "Hint   "+cleanDisplay(err.Hint))
		if next != "" {
			fmt.Fprintln(stderr, "Next   "+next)
		}
		return
	}
	envelope := struct {
		OK          bool         `json:"ok"`
		Error       *apiError    `json:"error"`
		Retries     int          `json:"retried"`
		Breadcrumbs []breadcrumb `json:"breadcrumbs,omitempty"`
	}{Error: &err, Retries: retries}
	if next != "" {
		envelope.Breadcrumbs = []breadcrumb{{Action: "recover", Command: next}}
	}
	_ = json.NewEncoder(out).Encode(envelope)
}

func recoverCommand(code string, args []string, profile string) (next, hint string) {
	suffix := profileOption(profile)
	switch code {
	case "usage":
		return safeHelpCommand(args), "Check the arguments and flags in command help."
	case "auth", "invalid_grant", "access_denied", "expired_token":
		if len(args) > 0 && args[0] == "doctor" {
			return "valiido auth status" + suffix, "Check or refresh the selected login."
		}
		return "valiido auth login" + suffix, "Sign in to the selected profile."
	case "forbidden", "not_authorized", "read_only_record", "example_not_allowed", "guide_chapter_not_allowed":
		return "valiido auth status" + suffix, "Check the selected profile and ask a Valiido administrator to verify your access."
	case "not_found", "conflict", "duplicate", "already_exists":
		noun, _ := resolveNoun(args)
		if isResource(noun) {
			return "valiido " + displayNoun(noun) + " list" + suffix, "Check the current records before trying again."
		}
		return "valiido resource --help", "Check the record and its current version before trying again."
	case "invalid_cursor":
		return safeHelpCommand(args), "Restart the list or use the load_more command from the same query and profile."
	case "invalid_file_reference":
		return "valiido documents upload --help", "Upload the original file again and use the new attachment identifier."
	case "invalid_input", "validation_failed", "unsupported_resource_attributes", "unknown_command", "unknown_resource_type", "unsupported_resource_action":
		return "valiido command list" + suffix, "Check the action's input schema and correct the input before retrying."
	case "network", "server_unavailable", "protocol", "api":
		return "valiido doctor" + suffix, "Check the configured connection and server."
	case "storage":
		return safeHelpCommand(args), "Check the file or directory and its permissions."
	case "busy", "rate_limited", "slow_down", "too_many_pending":
		return "valiido doctor" + suffix, "Wait for the current operation or limit to clear."
	case "no_url":
		return "valiido resource read --help", "Read the record in the CLI."
	case "browser":
		return "valiido open --help", "Run the command with --json to copy the browser URL."
	case "cancelled":
		return safeHelpCommand(args), "Check the result before repeating a change."
	default:
		return safeHelpCommand(args), "Check command help and try again after resolving the error."
	}
}

func safeHelpCommand(args []string) string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "valiido --help"
	}
	noun, depth := resolveNoun(args)
	path := displayNoun(noun)
	if _, ok := standaloneCommands[noun]; ok {
		return "valiido " + path + " --help"
	}
	if !knownNoun(noun) {
		return "valiido --help"
	}
	if len(args) > depth && validShortcut(noun, args[depth]) {
		path += " " + args[depth]
	}
	return "valiido " + path + " --help"
}

func exitCode(code string) int {
	switch code {
	case "usage":
		return 2
	case "unknown_command", "invalid_input", "validation_failed", "unsupported_resource_attributes",
		"invalid_format", "invalid_file_reference", "placeholder_email_not_allowed", "invalid_cursor",
		"invalid_reference_type", "unknown_resource_type", "unsupported_resource_action",
		"unsupported_delete_strategy", "unsupported_commentable", "ambiguous_assignment":
		return 6
	case "auth", "invalid_grant", "access_denied", "expired_token":
		return 3
	case "forbidden", "not_authorized", "read_only_record", "example_not_allowed", "guide_chapter_not_allowed":
		return 4
	case "not_found", "template_not_found", "example_not_found", "guide_item_not_found":
		return 5
	case "rate_limited", "slow_down", "too_many_pending", "server_unavailable", "busy":
		return 7
	case "storage":
		return 8
	case "conflict", "duplicate", "already_copied", "already_exists", "already_has_access", "already_in_progress":
		return 9
	default:
		return 1
	}
}
