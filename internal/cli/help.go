package cli

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/tabwriter"
)

type helpEntry struct {
	name, description string
}

var recordHelp = []helpEntry{
	{"comments", "List, create, and delete comments"},
	{"documents upload", "Upload an attachment"},
	{"<noun> import", "Import records from JSON"},
	{"<noun> pdf", "Download a record as PDF where supported"},
}

var topicHelp = []helpEntry{
	{"output", "Output formats and filtering"},
	{"exit-codes", "Exit status reference"},
	{"environment", "Environment variable reference"},
}

var outputHelp = []helpEntry{
	{"--json", "Print the JSON envelope and breadcrumbs"},
	{"--quiet", "Print JSON data only"},
	{"--jq EXPR", "Filter the JSON envelope with jq"},
	{"--ids-only", "Print one ID per line"},
	{"--count", "Print the result count"},
	{"--markdown", "Print Markdown"},
	{"--styled", "Force terminal formatting"},
}

var helpFlag = helpEntry{"-h, --help", "Show help"}
var profileFlag = helpEntry{"--profile NAME", "Select a credential profile"}

var help string

func writeMainHelp(stdout io.Writer) {
	fmt.Fprint(stdout, help)
}

func mainHelp() string {
	var out strings.Builder
	out.WriteString("Manage your Valiido ISMS from the terminal.\n\nUSAGE\n  valiido <command> [flags]\n")
	helpTable(&out, "ISMS", mainResources())
	helpTable(&out, "WORK WITH RECORDS", append(commandSection("records"), recordHelp...))
	helpTable(&out, "ACCOUNT & SYSTEM", commandSection("system"))
	helpTable(&out, "ADVANCED", commandSection("advanced"))
	helpTable(&out, "HELP TOPICS", topicHelp)
	flags := inheritedFlags(true)
	flags = append(flags, helpFlag, helpEntry{"-v, --version", "Show the installed version"})
	helpTable(&out, "FLAGS", flags)
	out.WriteString("\nEXAMPLES\n  $ valiido documents list --query policy\n  $ valiido risks show 42 --json\n")
	out.WriteString("\nLEARN MORE\n  valiido <noun> --help         Show actions and subcommands\n  valiido <noun> <verb> --help  Show arguments and flags\n  valiido help <topic>         Read a help topic\n  valiido more                 List all ISMS areas\n  valiido command list         Browse the live catalog and input schemas\n")
	return out.String()
}

func inheritedFlags(withProfile bool) []helpEntry {
	if withProfile {
		return append([]helpEntry{profileFlag}, outputHelp...)
	}
	return outputHelp
}

const liveActionsHelp = "  valiido command list  Show live actions and schemas\n"
const breadcrumbHelp = "  Follow breadcrumbs in the JSON envelope (--json).\n"

func helpTable(out *strings.Builder, title string, entries []helpEntry) {
	fmt.Fprintf(out, "\n%s\n", title)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, entry := range entries {
		fmt.Fprintf(table, "  %s\t%s\n", entry.name, entry.description)
	}
	table.Flush()
}

func helpRequested(args []string) bool {
	return len(args) > 1 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h")
}

func resourceHelp(noun string) []helpEntry {
	if !isResource(noun) {
		return nil
	}
	verbs := []helpEntry{resourceVerb(noun, "list"), resourceVerb(noun, "show")}
	if noun != "auditmagic_fullaudits" && noun != "employees" {
		verbs = append(verbs, resourceVerb(noun, "create"), resourceVerb(noun, "update"))
	}
	if noun != "auditmagic_fullaudits" && noun != "access_right_checklist_items" {
		verbs = append(verbs, resourceVerb(noun, "delete"))
	}
	if noun != "auditmagic_fullaudits" && noun != "employees" && noun != "kpi_measurements" {
		verbs = append(verbs, resourceVerb(noun, "import"))
	}
	switch noun {
	case "documents", "assets", "audits", "tasks", "risks", "vendors", "projects", "incidents", "threats",
		"management_reviews", "kpis", "requirements", "change_managements", "assethandovers",
		"boardings", "access_rights", "employee_roles":
		verbs = append(verbs, resourceVerb(noun, "examples"))
	}
	verbs = append(verbs, resourceVerb(noun, "pdf"))
	if command, ok := commandsByName[noun+" upload"]; ok {
		verbs = append(verbs, helpEntry{"upload", command.description})
	}
	for _, action := range specialActions[noun] {
		if _, listed := findAction(verbs, action.name); !listed {
			verbs = append(verbs, helpEntry{displayVerb(action.name), action.description})
		}
	}
	return verbs
}

func resourceVerb(noun, verb string) helpEntry {
	if command, ok := commandsByName[noun+" "+verb]; ok {
		return helpEntry{verb, command.description}
	}
	action, _ := findAction(resourceActions, verb)
	return action
}

func knownNoun(noun string) bool {
	return nounDescription(noun) != "" || specialActions[noun] != nil
}

func nounDescription(noun string) string {
	noun = canonicalNoun(noun)
	for _, resource := range resourceNames {
		if resource.server == noun {
			return resource.description
		}
	}
	if group, ok := findCommandGroup(noun); ok {
		return group.description
	}
	for _, entry := range recordHelp {
		if canonicalNoun(entry.name) == noun {
			return entry.description
		}
	}
	return ""
}

func helpVerbs(noun string) []helpEntry {
	if verbs := resourceHelp(noun); verbs != nil {
		return verbs
	}
	group, ok := findCommandGroup(noun)
	if !ok {
		var verbs []helpEntry
		for _, action := range specialActions[noun] {
			verbs = append(verbs, helpEntry{displayVerb(action.name), action.description})
		}
		return verbs
	}
	if group.standalone {
		return nil
	}
	var verbs []helpEntry
	for _, definition := range group.commands {
		_, verb, _ := strings.Cut(definition.name, " ")
		verbs = append(verbs, helpEntry{verb, definition.description})
	}
	return verbs
}

func contextualHelp(args []string) (string, bool) {
	if !helpRequested(args) {
		return "", false
	}
	noun, depth := resolveNoun(args)
	invoked := strings.Join(args[:depth], " ")
	if isResource(noun) {
		invoked = displayNoun(noun)
	}
	description := nounDescription(noun)
	if description == "" && specialActions[noun] != nil {
		description = "Run " + displayNoun(noun) + " actions"
	}
	if description == "" {
		return "", false
	}
	verbs := helpVerbs(noun)
	var out strings.Builder
	if command, ok := standaloneCommands[noun]; ok && (len(args) == depth+1 || noun != "doctor" || args[depth] == "run") {
		writeLeafHelp(&out, description, command.usage, command.name, "")
		return out.String(), true
	}
	if len(args) == depth+1 {
		if len(verbs) == 0 {
			return "", false
		}
		writeGroupHelp(&out, invoked, noun, description, verbs)
		return out.String(), true
	}
	verb := displayVerb(args[depth])
	description = actionDescription(noun, verb, verbs)
	if description == "" {
		return "", false
	}
	usage := leafUsage(invoked, noun, verb, args[depth+1:len(args)-1])
	writeLeafHelp(&out, description, usage, noun+" "+canonicalVerb(verb), invoked)
	return out.String(), true
}

func actionDescription(noun, verb string, verbs []helpEntry) string {
	for _, entry := range verbs {
		if entry.name == verb {
			return entry.description
		}
	}
	if !validShortcut(noun, verb) {
		return ""
	}
	if action, ok := findAction(resourceActions, verb); ok {
		return action.description
	}
	return "Run " + verb
}

func writeGroupHelp(out *strings.Builder, invoked, noun, description string, verbs []helpEntry) {
	fmt.Fprintf(out, "%s\n\nUSAGE\n  valiido %s <verb> [flags]\n", description, invoked)
	helpTable(out, "COMMANDS", verbs)
	children := resourceChildren(displayNoun(noun))
	if len(children) > 0 {
		helpTable(out, "SUBCOMMANDS", children)
	}
	if alias := nounAliases[noun]; alias != "" {
		fmt.Fprintf(out, "\nALIASES\n  %s\n", alias)
	}
	helpTable(out, "FLAGS", []helpEntry{helpFlag})
	helpTable(out, "INHERITED FLAGS", inheritedFlags(true))
	example := exampleCommand(leafUsage(invoked, noun, verbs[0].name, nil))
	fmt.Fprintf(out, "\nEXAMPLES\n  $ %s\n", example)
	fmt.Fprintf(out, "\nLEARN MORE\n  valiido %s <verb> --help\n", invoked)
	if len(children) > 0 {
		fmt.Fprintf(out, "  valiido %s <subcommand> --help\n", invoked)
	}
	if resourceHelp(noun) != nil {
		out.WriteString(liveActionsHelp)
	}
	out.WriteString(breadcrumbHelp)
}

func leafUsage(invoked, noun, verb string, args []string) string {
	prefix := "valiido " + invoked + " " + verb
	if noun == "comments" && verb == "list" {
		return prefix + " (--input JSON | --input-file FILE.json)"
	}
	shortcut := resourceShortcut(append([]string{noun, verb}, args...))
	name := canonicalNoun(shortcut[0]) + " " + shortcut[1]
	usage := commandUsage(name)
	switch {
	case name == "commands run" && shortcut[0] != noun:
		if !strings.HasPrefix(canonicalVerb(verb), "list") {
			usage = strings.Replace(usage, " [--limit N] [--cursor TOKEN]", "", 1)
		}
		return prefix + strings.TrimPrefix(usage, "valiido command run NAME")
	case shortcut[0] == "resource" && noun != "resources":
		return prefix + strings.TrimPrefix(usage, "valiido resource "+shortcut[1]+" TYPE")
	default:
		_, tail, _ := strings.Cut(usage, " "+shortcut[1])
		return prefix + tail
	}
}

var optionPattern = regexp.MustCompile(`--[a-z][a-z-]*(?: [A-Z][A-Z0-9]*(?:\.[a-z]+)?)?`)

var optionDescriptions = map[string]string{
	"agent": "Choose codex, claude, or all", "force": "Replace the existing skill",
	"server": "Set the server origin", "file-credentials": "Allow a mode-0600 credential file",
	"query": "Filter results", "text": "Print the document as plain text",
	"output": "Write PDF to this file", "limit": "Return at most N results",
	"cursor": "Resume a list using its next_cursor", "page-size": "Maximum results per request (1–250)",
	"file": "Read a JSON array from this file", "dry-run": "Preview without applying changes",
	"input": "Pass a JSON object", "input-file": "Read a JSON object from this file",
	"check": "Only report whether a newer version exists",
}

func writeLeafHelp(out *strings.Builder, description, usage, name, parent string) {
	positional, _, _ := strings.Cut(usage, " [--")
	positional, _, _ = strings.Cut(positional, " (--")
	fmt.Fprintf(out, "%s\n\nUSAGE\n  %s [flags]\n", description, positional)
	local := []helpEntry{helpFlag}
	for _, option := range optionPattern.FindAllString(usage, -1) {
		key, _, _ := strings.Cut(strings.TrimPrefix(option, "--"), " ")
		if label := optionDescriptions[key]; label != "" {
			local = append(local, helpEntry{option, label})
		}
	}
	helpTable(out, "FLAGS", local)
	if name != "shell-completion" && name != "more" {
		helpTable(out, "INHERITED FLAGS", inheritedFlags(commandSupportsProfile(name)))
	}
	fmt.Fprintf(out, "\nEXAMPLES\n  $ %s\n", exampleCommand(usage))
	switch name {
	case "commands confirm", "commands reject":
		out.WriteString("\n  Pass - to read a preparation result or a single token from stdin.\n")
		out.WriteString("  $ valiido tasks update --input-file patch.json --json | valiido command " + strings.TrimPrefix(name, "commands ") + " -\n")
	case "open":
		out.WriteString("\n  Opens a browser in a terminal. Use --json to print the URL for a script.\n")
	case "shell-completion":
		out.WriteString("  $ source <(valiido shell-completion bash)\n")
	case "documents show":
		out.WriteString("  $ valiido documents show 42 --text\n  $ valiido documents show 42 --output synthetic.pdf\n")
	case "documents upload":
		out.WriteString("\n  Returns attachment identifiers. Use --json to read them.\n")
	case "auth login":
		out.WriteString("\n  Approve the pairing code shown on stderr.\n")
	}
	out.WriteString("\nLEARN MORE\n")
	if parent != "" {
		fmt.Fprintf(out, "  valiido %s --help\n", parent)
		noun, _ := resolveNoun(strings.Fields(parent))
		if isResource(noun) || noun == "comments" {
			out.WriteString(liveActionsHelp)
		}
	} else {
		out.WriteString("  valiido --help\n")
	}
}

func exampleCommand(usage string) string {
	base, _, _ := strings.Cut(usage, " [--")
	base, _, _ = strings.Cut(base, " (--")
	example := strings.NewReplacer("TYPE", "documents", " NOUN", " documents", " ID", " 42", " FILE.json", " synthetic.json",
		" FILE.pdf", " synthetic.pdf", " FILE", " synthetic.pdf",
		" NAME", " assets.list", " TOKEN|-", " CONFIRMATION_TOKEN", "bash|zsh|fish", "bash").Replace(base)
	switch {
	case strings.Contains(example, " import"):
		return example + " --dry-run"
	case strings.HasSuffix(example, " pdf 42"):
		return example + " --output synthetic.pdf"
	case strings.HasSuffix(example, " upload synthetic.pdf"):
		return example + " --dry-run"
	case strings.HasSuffix(example, "comments list"):
		return example + ` --input '{"resource_type":"documents","resource_id":42}'`
	case strings.HasSuffix(example, "comments create"):
		return example + ` --input '{"resource_type":"documents","resource_id":42,"body":"Synthetic comment"}' --dry-run`
	case strings.HasSuffix(example, "comments delete"):
		return example + " --input-file comment.json --dry-run"
	case strings.HasSuffix(example, " create"), strings.HasSuffix(example, " update"), strings.HasSuffix(example, " delete"):
		return example + " --input-file record.json --dry-run"
	case strings.HasSuffix(example, " run assets.list"):
		return example + ` --input '{"limit":5}'`
	}
	return example
}

func nounChoices(noun string) []string {
	return append(entryNames(helpVerbs(noun)), entryNames(resourceChildren(displayNoun(noun)))...)
}

func rootChoices() []string {
	entries := append(allResourceAreas(), commandSection("system")...)
	entries = append(entries, commandSection("advanced")...)
	entries = append(entries, commandSection("records")...)
	choices := append(entryNames(entries), "comments")
	return append(choices, entryNames(commandSection("utility"))...)
}

func entryNames(entries []helpEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.name)
	}
	return names
}

func showHelp(call *invocation) error {
	if len(call.args) == 0 {
		writeMainHelp(call.output)
		return nil
	}
	if topic, ok := helpTopics[call.args[0]]; len(call.args) == 1 && ok {
		fmt.Fprintln(call.output, topic)
		return nil
	}
	if body, ok := contextualHelp(append(append([]string(nil), call.args...), "--help")); ok {
		fmt.Fprint(call.output, body)
		return nil
	}
	return unknownHelpCommand(append([]string{call.command.name}, call.args...))
}

func showAreas(call *invocation) error {
	if len(call.args) != 0 {
		return unknownHelpCommand([]string{call.command.name})
	}
	var list strings.Builder
	helpTable(&list, "ISMS", allResourceAreas())
	list.WriteString("\nRun valiido <noun> --help for actions and subcommands.\n")
	fmt.Fprint(call.output, strings.TrimPrefix(list.String(), "\n"))
	return nil
}
