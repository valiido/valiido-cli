package cli

import (
	"context"
	"io"
	"strconv"
	"strings"
	"unicode"
)

type commandDefinition struct {
	name, usage, description string
	run                      func(*invocation) error
}

type commandGroup struct {
	display, description, section string
	standalone                    bool
	commands                      []commandDefinition
}

var commandGroups []commandGroup
var commandDefinitions []commandDefinition
var commandsByName map[string]commandDefinition
var standaloneCommands map[string]commandDefinition

func init() {
	commandGroups = []commandGroup{
		{"help", "", "utility", true, []commandDefinition{
			{"help", "valiido --help", "", showHelp},
		}},
		{"more", "List the ISMS areas", "utility", true, []commandDefinition{
			{"more", "valiido more", "", showAreas},
		}},
		{"auth", "Sign in, check login, or sign out", "system", false, []commandDefinition{
			{"auth login", "valiido auth login [--profile NAME] [--server URL] [--file-credentials] [--dry-run]", "Sign in with a device code", loginProfile},
			{"auth status", "valiido auth status [--profile NAME]", "Check the current login", showProfile},
			{"auth logout", "valiido auth logout [--profile NAME] [--dry-run]", "Revoke the current login", logoutProfile},
		}},
		{"setup", "Install the Valiido agent skill", "system", true, []commandDefinition{
			{"setup", "valiido setup [--agent codex|claude|all] [--force] [--dry-run]", "", runSetup},
		}},
		{"doctor", "Check login and server connectivity", "system", true, []commandDefinition{
			{"doctor run", "valiido doctor [--profile NAME]", "", runDoctor},
		}},
		{"shell-completion", "Generate Bash, Zsh, or Fish completion", "system", true, []commandDefinition{
			{"shell-completion", "valiido shell-completion bash|zsh|fish", "", generateCompletion},
		}},
		{"version", "Show the installed version", "system", true, []commandDefinition{
			{"version", "valiido version", "", showVersion},
		}},
		{"upgrade", "Update valiido to the latest release", "system", true, []commandDefinition{
			{"upgrade", "valiido upgrade [--check] [--dry-run]", "", runUpgrade},
		}},
		{"open", "Open a record in the browser", "records", true, []commandDefinition{
			{"open", "valiido open NOUN ID [--profile NAME]", "", openRecord},
		}},
		{"resource", "Use generic resource operations", "advanced", false, []commandDefinition{
			{"resources list", "valiido resource list TYPE [--query TEXT] [--limit N] [--cursor TOKEN] [--page-size N] [--profile NAME] [--json]", "List records by type", listResource},
			{"resources show", "valiido resource show TYPE ID [--profile NAME] [--json]", "Show a record by type and ID", readResource},
			{"resources examples", "valiido resource examples TYPE [--query TEXT] [--limit N] [--cursor TOKEN] [--page-size N] [--profile NAME] [--json]", "Browse examples by type", listExamples},
			{"resources import", "valiido resource import TYPE --file FILE.json [--dry-run] [--profile NAME]", "Import a JSON array", importResource},
			{"resources pdf", "valiido resource pdf TYPE ID --output FILE.pdf [--dry-run] [--profile NAME]", "Download a record as PDF", saveResourcePDF},
		}},
		{"command", "Discover actions, run them, or confirm changes", "advanced", false, []commandDefinition{
			{"commands list", "valiido command list [--query TEXT] [--limit N] [--profile NAME] [--json]", "Browse the live catalog and input schemas", listCommands},
			{"commands run", "valiido command run NAME [--input JSON | --input-file FILE.json] [--dry-run] [--limit N] [--cursor TOKEN] [--profile NAME]", "Run a named action", invokeCommand},
			{"commands confirm", "valiido command confirm TOKEN|- [--profile NAME]", "Apply a pending change", confirmCommand},
			{"commands reject", "valiido command reject TOKEN|- [--profile NAME]", "Discard a pending change", rejectCommand},
		}},
		{"api", "Fetch the API schema", "advanced", false, []commandDefinition{
			{"api schema", "valiido api schema [--profile NAME]", "Fetch the API schema", showSchema},
		}},
		{"documents", "", "", false, []commandDefinition{
			{"documents list", "valiido documents list [--query TEXT] [--limit N] [--cursor TOKEN] [--page-size N] [--profile NAME] [--json]", "List documents and folders", listDocuments},
			{"documents show", "valiido documents show ID [--text | --output FILE.pdf] [--dry-run] [--profile NAME] [--json]", "Show a document", runDocumentRead},
			{"documents upload", "valiido documents upload FILE [--dry-run] [--profile NAME]", "Upload an attachment", uploadDocument},
		}},
		{"tasks", "", "", false, []commandDefinition{
			{"tasks list", "valiido tasks list [--query TEXT] [--limit N] [--cursor TOKEN] [--page-size N] [--profile NAME] [--json]", "List tasks", listTasks},
			{"tasks show", "valiido tasks show ID [--profile NAME] [--json]", "Show a task", readTask},
		}},
		{"risks", "", "", false, []commandDefinition{
			{"risks list", "valiido risks list [--query TEXT] [--limit N] [--cursor TOKEN] [--page-size N] [--profile NAME] [--json]", "List risks", listRisks},
			{"risks show", "valiido risks show ID [--profile NAME] [--json]", "Show a risk", readRisk},
		}},
		{"_complete", "", "", true, []commandDefinition{
			{"_complete", "", "", completeCommand},
		}},
	}
	indexCommands()
	help = mainHelp()
}

func indexCommands() {
	commandsByName = make(map[string]commandDefinition)
	standaloneCommands = make(map[string]commandDefinition)
	for _, group := range commandGroups {
		for _, command := range group.commands {
			commandsByName[command.name] = command
			commandDefinitions = append(commandDefinitions, command)
		}
		if group.standalone {
			standaloneCommands[group.display] = group.commands[0]
		}
	}
}

func commandSection(section string) []helpEntry {
	var entries []helpEntry
	for _, group := range commandGroups {
		if group.section == section {
			entries = append(entries, helpEntry{group.display, group.description})
		}
	}
	return entries
}

func findCommandGroup(noun string) (commandGroup, bool) {
	for _, group := range commandGroups {
		if canonicalNoun(group.display) == noun {
			return group, true
		}
	}
	return commandGroup{}, false
}

func commandUsage(name string) string {
	if command, ok := commandsByName[name]; ok {
		return command.usage
	}
	return "valiido --help"
}

type invocation struct {
	context       context.Context
	output        *output
	stderr        io.Writer
	command       commandDefinition
	args          []string
	resourceAlias bool
}

func routeCommand(args []string) (commandDefinition, []string, bool, error) {
	originalNoun := args[0]
	if strings.HasPrefix(originalNoun, "-") {
		return commandDefinition{}, nil, false, usageError("Options must follow a command. Run valiido --help.")
	}
	if command, ok := standaloneCommands[originalNoun]; ok {
		return command, args[1:], false, nil
	}
	noun, depth := resolveNoun(args)
	if err := validateCommandPath(args, noun, depth); err != nil {
		return commandDefinition{}, nil, false, err
	}
	args = resourceShortcut(append([]string(nil), args...))
	resourceAlias := args[0] == "resource" && originalNoun != args[0]
	args[0] = canonicalNoun(args[0])
	if len(args) < 2 {
		return commandDefinition{}, nil, false, missingAction(originalNoun)
	}
	command, ok := commandsByName[args[0]+" "+args[1]]
	if !ok {
		return commandDefinition{}, nil, false, unknownHelpCommand(args)
	}
	return command, args[2:], resourceAlias, nil
}

func validateCommandPath(args []string, noun string, depth int) error {
	if !knownNoun(noun) {
		return unknownHelpCommand(args)
	}
	if reservedNoun(noun) {
		return nil
	}
	if len(args) == depth || strings.HasPrefix(args[depth], "-") {
		return missingAction(displayNoun(noun))
	}
	if !validShortcut(noun, args[depth]) {
		return unknownHelpCommand(args)
	}
	if canonicalVerb(args[depth]) == "create" && len(args) > depth+1 && !strings.HasPrefix(args[depth+1], "-") {
		return usageError("Create takes no record ID. Pass fields with --input JSON or --input-file FILE.json.")
	}
	return nil
}

func missingAction(noun string) error {
	return usageError("Expected an action. Run valiido " + noun + " --help.")
}

// Display paths and Rails resource types are independent. The resource type
// remains a working shortcut, even when its public path is nested.
var resourceNames = []struct {
	path, server, description string
}{
	{"documents", "documents", "Policies, procedures and folders"},
	{"tasks", "tasks", "Tasks and recurring work"},
	{"risks", "risks", "Risk records"},
	{"assets", "assets", "Information assets"},
	{"audits", "audits", "Audit records"},
	{"incidents", "incidents", "Security incidents"},
	{"vendors", "vendors", "Suppliers and vendors"},
	{"requirements", "requirements", "Compliance requirements"},
	{"management-reviews", "management_reviews", "Management reviews"},
	{"change-requests", "change_managements", "Change requests and approvals"},
	{"kpis", "kpis", "Performance indicators"},
	{"employees", "employees", "Employee directory"},
	{"full-audits", "auditmagic_fullaudits", "Full audits and reports"},
	{"threats", "threats", "Threat records"},
	{"projects", "projects", "ISMS projects"},
	{"employee-roles", "employee_roles", "Employee roles"},
	{"asset-handovers", "assethandovers", "Equipment handover categories"},
	{"boardings", "boardings", "Onboarding and offboarding plans"},
	{"access-rights", "access_rights", "Access right templates"},
	{"kpis measurements", "kpi_measurements", "KPI measurements"},
	{"asset-handovers items", "assethandover_items", "Equipment handover items"},
	{"boardings steps", "boarding_steps", "Boarding plan steps"},
	{"boardings checklists", "boarding_checklists", "Boarding checklists"},
	{"boardings checklists items", "boarding_checklist_items", "Boarding checklist items"},
	{"access-rights items", "access_right_items", "Access right items"},
	{"access-rights checklists", "access_right_checklists", "Access right checklists"},
	{"access-rights checklists items", "access_right_checklist_items", "Access right checklist items"},
}

func mainResources() []helpEntry {
	return allResourceAreas()[:12]
}

func allResourceAreas() []helpEntry {
	var entries []helpEntry
	for _, resource := range resourceNames {
		if !strings.Contains(resource.path, " ") {
			entries = append(entries, helpEntry{resource.path, resource.description})
		}
	}
	return entries
}

func resourceChildren(parent string) []helpEntry {
	var entries []helpEntry
	for _, resource := range resourceNames {
		child, ok := strings.CutPrefix(resource.path, parent+" ")
		if ok && !strings.Contains(child, " ") {
			entries = append(entries, helpEntry{child, resource.description})
		}
	}
	return entries
}

func displayNoun(noun string) string {
	for _, resource := range resourceNames {
		if resource.server == noun {
			return resource.path
		}
	}
	if alias, ok := nounAliases[noun]; ok {
		return alias
	}
	return noun
}

func resolveNoun(args []string) (string, int) {
	// Prefer the longest matching public path. The first word may also be the
	// resource type, as in "assethandovers items".
	for depth := min(3, len(args)); depth > 1; depth-- {
		path := strings.Join(append([]string{displayNoun(canonicalNoun(args[0]))}, args[1:depth]...), " ")
		for _, resource := range resourceNames {
			if resource.path == path {
				return resource.server, depth
			}
		}
	}
	return canonicalNoun(args[0]), 1
}

func isResource(noun string) bool {
	for _, resource := range resourceNames {
		if resource.server == noun {
			return true
		}
	}
	return false
}

// Verbs use the server's snake_case action names. Kebab-case spellings and
// read are aliases; help shows the kebab-case form.
func canonicalVerb(verb string) string {
	if verb == "read" {
		return "show"
	}
	return strings.ReplaceAll(verb, "-", "_")
}

func displayVerb(verb string) string {
	return strings.ReplaceAll(canonicalVerb(verb), "_", "-")
}

// These are CLI spellings. Rails still decides which actions a resource and
// the authenticated caller support. Newly added actions can use command run.
var resourceActions = []helpEntry{
	{"list", "List records"}, {"show", "Show a record by ID"}, {"create", "Create a record"},
	{"update", "Update a record"}, {"delete", "Delete a record"}, {"examples", "Browse examples"},
	{"import", "Import a JSON array"}, {"pdf", "Download a PDF"},
	{"bulk_create", "Create several records"}, {"bulk_delete", "Move several records to trash"},
	{"history", "Read recent change history"}, {"list_examples", "List examples that can be copied"},
	{"copy_from_example", "Copy one example"}, {"bulk_copy_from_examples", "Copy several examples"},
	{"list_deleted", "List discarded records"}, {"restore", "Restore a discarded record"},
	{"duplicate", "Duplicate a record"}, {"destroy_permanently", "Permanently delete a discarded record"},
	{"empty_trash", "Permanently delete all discarded records"},
}

// Agent shortcuts with resource-specific verbs from the Rails command
// registry. Descriptions follow the live catalog (valiido command list).
var specialActions = map[string][]helpEntry{
	"tasks": {
		{"toggle_complete", "Toggle completion for one task"},
		{"bulk_delete", "Move several tasks to trash"},
	},
	"employees": {
		{"directory", "List employees and roles available for assignment"},
		{"list_access", "List Valiido access and pending invitations"},
		{"create", "Create one employee profile"},
		{"update_profile", "Update an employee profile without changing access"},
		{"grant_access", "Invite an employee to Valiido"},
		{"revoke_access", "Deactivate one employee's Valiido access"},
		{"change_access_level", "Change an employee's Valiido access level"},
		{"archive", "Archive one employee without Valiido access"},
		{"unarchive", "Bring one archived employee back to the list"},
		{"anonymize", "Remove the personal data of one archived employee"},
	},
	"access_levels":                {{"list", "List the Valiido access levels and their permissions"}},
	"boarding_checklists":          {{"start_from_template", "Start a boarding checklist from a plan template"}},
	"access_right_checklists":      {{"start_from_template", "Start an access right review from a template"}},
	"boarding_checklist_items":     {{"toggle_complete", "Toggle completion for one checklist item"}, {"move", "Move one checklist item up or down"}},
	"access_right_checklist_items": {{"toggle_complete", "Toggle completion for one checklist item"}, {"move", "Move one checklist item up or down"}},
	"boarding_steps":               {{"move", "Move one boarding step up or down"}},
	"assethandover_items": {
		{"assign", "Assign one item to an employee"},
		{"return", "Return one assigned item to stock"},
		{"empty_trash", "Permanently delete discarded items from one handover"},
	},
	"boardings":        {{"copy_plan_from_example", "Copy a boarding plan and its steps from examples"}},
	"access_rights":    {{"copy_set_from_example", "Copy an access right set and its items from examples"}},
	"frameworks":       {{"list", "List frameworks and the current selection"}, {"select", "Select a framework or variant"}, {"deselect", "Deselect a framework or variant"}},
	"guide_frameworks": {{"toggle", "Toggle a guide framework in scope"}},
	"guide_items": {
		{"list", "List guide items"},
		{"next", "Find the next open guide chapter or todo"},
		{"show", "Show one guide item"},
		{"search", "Search guide chapters, tasks and requirements"},
		{"set_completion", "Set the status of one guide task"},
		{"set_favorite", "Star or unstar one guide chapter"},
	},
	"references": {
		{"create", "Link two ISMS records"},
		{"delete", "Remove a link between two records"},
		{"list", "List links of one record"},
		{"search", "Search records that can be linked"},
	},
	"comments": {{"create", "Add a comment"}, {"delete", "Delete a comment"}, {"list", "List comments on a record"}},
	"chats": {
		{"list", "List your AuditMagic chats"},
		{"show", "Show messages and pending approvals in one chat"},
		{"create", "Create a chat and optionally send a first message"},
		{"send_message", "Send a message and start a response"},
		{"decide_tool_approval", "Approve or decline pending tool calls"},
		{"cancel", "Cancel the running response"},
		{"retry", "Retry the latest failed response"},
	},
	"auditmagic": {
		{"ask_support", "Send open findings about a record to Valiido Support"},
		{"complete_audit", "Start a full AuditMagic audit"},
		{"set_finding_completion", "Resolve or reopen a finding"},
		{"list_objects", "List records with open findings"},
		{"list_remembered_findings", "List findings marked not to be raised again"},
		{"resource_status", "Show review status and findings for one record"},
		{"review_resource", "Review one record again now"},
	},
	"changelog": {{"list", "List recent changelog entries"}, {"show", "Show one changelog entry"}},
}

func findAction(actions []helpEntry, verb string) (helpEntry, bool) {
	verb = canonicalVerb(verb)
	for _, action := range actions {
		if action.name == verb {
			return action, true
		}
	}
	return helpEntry{}, false
}

func validShortcut(noun, verb string) bool {
	verb = canonicalVerb(verb)
	if _, fixed := commandsByName[noun+" "+verb]; fixed {
		return true
	}
	if _, ok := findAction(specialActions[noun], verb); ok {
		return true
	}
	_, ok := findAction(resourceActions, verb)
	return ok && isResource(noun)
}

func canonicalNoun(noun string) string {
	for _, resource := range resourceNames {
		if resource.path == noun && !strings.Contains(noun, " ") {
			return resource.server
		}
	}
	for canonical, alias := range nounAliases {
		if alias == noun {
			return canonical
		}
	}
	return noun
}

var nounAliases = map[string]string{
	"documents": "document",
	"risks":     "risk",
	"tasks":     "task",
	"resources": "resource",
	"commands":  "command",
}

func unknownHelpCommand(args []string) error {
	word, choices := commandChoices(args)
	label := word
	if len(word) > 72 || strings.ContainsFunc(word, unicode.IsControl) {
		label = strconv.Quote(fitDisplay(cleanDisplay(word), 72))
	}
	err := failure("usage", "Unknown command "+label+".")
	err.Hint = suggestion(word, choices)
	return err
}

func commandChoices(args []string) (string, []string) {
	noun, depth := resolveNoun(args)
	if depth == len(args) || strings.HasPrefix(args[depth], "-") {
		return args[0], rootChoices()
	}
	switch {
	case noun == "help":
		return args[depth], append(entryNames(topicHelp), rootChoices()...)
	case knownNoun(noun):
		return args[depth], nounChoices(noun)
	}
	return args[0], rootChoices()
}

func suggestion(word string, choices []string) string {
	word = strings.ReplaceAll(word, "_", "-")
	for _, choice := range choices {
		if choice == word {
			return ""
		}
	}
	for _, choice := range choices {
		if oneEditAway(word, choice) {
			return "Did you mean " + choice + "?"
		}
	}
	return ""
}

// Include adjacent transpositions, a common command-line typo.
func oneEditAway(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	i := 0
	for i < len(a) && a[i] == b[i] {
		i++
	}
	if len(a) != len(b) {
		return a[i:] == b[i+1:]
	}
	if i == len(a) {
		return false
	}
	return a[i+1:] == b[i+1:] ||
		i+1 < len(a) && a[i] == b[i+1] && a[i+1] == b[i] && a[i+2:] == b[i+2:]
}
