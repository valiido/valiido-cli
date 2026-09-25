package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

func shellCompletion(w io.Writer, args []string) error {
	if len(args) != 1 {
		return failure("usage", "Use valiido shell-completion bash|zsh|fish.")
	}
	var script string
	switch args[0] {
	case "bash":
		script = bashCompletion
	case "zsh":
		script = zshCompletion
	case "fish":
		script = fishCompletion
	default:
		return failure("usage", "Use valiido shell-completion bash|zsh|fish.")
	}
	_, err := fmt.Fprint(w, script)
	return err
}

func complete(ctx context.Context, w io.Writer, path, profile, prefix string) error {
	if path == "--flags" {
		return writeChoices(w, completionFlags(""))
	}
	var choices []string
	if strings.HasPrefix(prefix, "-") {
		choices = completionFlags(path)
	} else {
		choices = staticChoices(path)
		if kind := completionResource(path); kind != "" {
			choices = append(choices, completeIDs(ctx, profile, kind)...)
		}
	}
	sort.Strings(choices)
	return writeChoices(w, choices)
}

func completionFlags(path string) []string {
	choices := []string{"--help"}
	if path == "" {
		return append(choices, "--version")
	}
	args := strings.Fields(path)
	def, _, _, err := routeCommand(args)
	if err != nil {
		return choices
	}
	switch def.name {
	case "help", "more", "shell-completion", "_complete":
		return choices
	}
	for _, entry := range inheritedFlags(commandSupportsProfile(def.name)) {
		name, _, _ := strings.Cut(entry.name, " ")
		choices = append(choices, name)
	}
	for _, flag := range optionPattern.FindAllString(def.usage, -1) {
		name, _, _ := strings.Cut(flag, " ")
		choices = append(choices, name)
	}
	return choices
}

func staticChoices(path string) []string {
	switch path {
	case "help":
		return append(entryNames(topicHelp), rootChoices()...)
	case "shell-completion":
		return []string{"bash", "zsh", "fish"}
	case "open":
		return entryNames(allResourceAreas())
	case "resource list", "resource show", "resource pdf", "resource examples", "resource import",
		"resources list", "resources show", "resources pdf", "resources examples", "resources import":
		choices := make([]string, 0, len(resourceNames))
		for _, noun := range resourceNames {
			choices = append(choices, noun.server)
		}
		return choices
	}
	if path == "" {
		return rootChoices()
	}
	args := strings.Fields(path)
	if len(args) == 0 {
		return nil
	}
	if args[0] == "open" && len(args) > 1 {
		return entryNames(resourceChildren(strings.Join(args[1:], " ")))
	}
	if args[0] == "help" && len(args) > 1 {
		args = args[1:]
	}
	group, depth := resolveNoun(args)
	if depth != len(args) {
		return nil
	}
	return nounChoices(group)
}

func completionResource(path string) string {
	args := strings.Fields(path)
	if len(args) < 2 {
		return ""
	}
	kind, depth := resolveNoun(args)
	switch {
	case kind == "open":
		kind, depth = resolveNoun(args[1:])
		if depth != len(args)-1 {
			return ""
		}
	case kind == "resources" && len(args) == 3 && (canonicalVerb(args[1]) == "show" || args[1] == "pdf"):
		kind = args[2]
	case len(args) == depth+1 && (canonicalVerb(args[depth]) == "show" || args[depth] == "pdf"):
	default:
		return ""
	}
	if !isResource(kind) {
		return ""
	}
	return kind
}

func completeIDs(ctx context.Context, profile, kind string) []string {
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	store, err := profileStore(profile, false)
	if err != nil {
		return nil
	}
	value, err := store.load()
	if err != nil || value.AccessToken == "" || !value.ExpiresAt.After(time.Now()) {
		return nil
	}
	client, err := newClient(value.Server, value.AccessToken)
	if err != nil {
		return nil
	}
	client.profile, client.session = profile, &value
	if _, err := client.me(ctx); err != nil {
		return nil
	}
	const limit = 25
	page, err := client.resources(ctx, kind, listOptions{limit: limit, pageSize: limit})
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(page.Resources))
	for _, item := range page.Resources {
		ids = append(ids, strconv.FormatInt(item.ID, 10))
	}
	return ids
}

func writeChoices(w io.Writer, choices []string) error {
	seen := map[string]bool{}
	for _, choice := range choices {
		if !seen[choice] {
			if _, err := fmt.Fprintln(w, choice); err != nil {
				return err
			}
			seen[choice] = true
		}
	}
	return nil
}

const bashCompletion = `_valiido_complete() {
  local word="${COMP_WORDS[COMP_CWORD]}" path="${COMP_WORDS[*]:1:COMP_CWORD-1}" profile=default i choices
  for ((i=1; i<COMP_CWORD; i++)); do
    case "${COMP_WORDS[i]}" in
      --profile) profile="${COMP_WORDS[i+1]:-default}" ;;
      --profile=*) profile="${COMP_WORDS[i]#--profile=}" ;;
    esac
  done
  choices=$(valiido _complete "$path" "$profile" "$word" 2>/dev/null)
  COMPREPLY=($(compgen -W "$choices" -- "$word"))
}
complete -F _valiido_complete valiido
`

const zshCompletion = `_valiido_complete() {
  local path="" profile=default i
  local -a choices
  for ((i=2; i<CURRENT; i++)); do
    case "$words[i]" in
      --profile) profile="${words[i+1]:-default}" ;;
      --profile=*) profile="${words[i]#--profile=}" ;;
    esac
  done
  if (( CURRENT > 2 )); then path="${(j: :)words[2,CURRENT-1]}"; fi
  choices=("${(@f)$(valiido _complete "$path" "$profile" "$PREFIX" 2>/dev/null)}")
  compadd -- "${choices[@]}"
}
compdef _valiido_complete valiido
`

const fishCompletion = `function __valiido_choices
    set -l words (commandline -opc)
    set -l profile default
    for i in (seq (count $words))
        switch $words[$i]
            case --profile
                if test $i -lt (count $words)
                    set profile $words[(math $i + 1)]
                end
            case '--profile=*'
                set profile (string replace -- '--profile=' '' $words[$i])
        end
    end
    set -l path (string join " " $words[2..-1])
	set -l prefix (commandline -ct)
	valiido _complete "$path" "$profile" "$prefix" 2>/dev/null
end
complete -c valiido -f -a '(__valiido_choices)'
`

func generateCompletion(call *invocation) error { return shellCompletion(call.output, call.args) }

func completeCommand(call *invocation) error {
	if len(call.args) < 2 || len(call.args) > 3 {
		return usageError("Expected a completion path and profile.")
	}
	prefix := ""
	if len(call.args) == 3 {
		prefix = call.args[2]
	}
	path := call.args[0]
	args := strings.Fields(path)
	if index := strings.Index(path, " --profile"); index >= 0 && len(args) > 1 {
		path = path[:index]
	}
	return complete(call.context, call.output, path, call.args[1], prefix)
}
