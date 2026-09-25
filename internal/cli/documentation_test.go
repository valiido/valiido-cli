package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/valiido/valiido-cli/skills"
)

func TestHelpIsGeneratedFromUniqueCommandDefinitions(t *testing.T) {
	seen := map[string]bool{}
	for _, definition := range commandDefinitions {
		if definition.name == "" || definition.run == nil || seen[definition.name] {
			t.Fatalf("invalid or duplicate command definition: %#v", definition)
		}
		seen[definition.name] = true
		if definition.usage == "" {
			continue
		}
		args := parserProbeArgs(definition.name)
		if definition.name != "help" && definition.name != "version" {
			if body, ok := contextualHelp(args); !ok || !strings.Contains(body, "\nUSAGE\n") {
				t.Fatalf("help omitted %q", definition.name)
			}
		}
	}
	if len(seen) != len(commandsByName) {
		t.Fatal("known command index drifted from command definitions")
	}
}

func TestEveryDocumentedFixedCommandIsAcceptedByTheParser(t *testing.T) {
	for _, definition := range commandDefinitions {
		if definition.usage == "" {
			continue
		}
		args := parserProbeArgs(definition.name)
		expected := help
		switch definition.name {
		case "help":
		case "version":
			expected = "valiido " + version + "\n"
		default:
			expected, _ = contextualHelp(args)
		}
		var output strings.Builder
		if code := Run(t.Context(), args, &output, &output); code != 0 || output.String() != expected {
			t.Errorf("documented command %q is not accepted by the parser", definition.usage)
		}
	}
}

func TestContextualHelpShowsDocumentedOptions(t *testing.T) {
	for _, definition := range commandDefinitions {
		if definition.usage == "" {
			continue
		}
		if definition.name == "help" || definition.name == "version" {
			continue
		}
		body, ok := contextualHelp(parserProbeArgs(definition.name))
		if !ok {
			t.Fatalf("missing help for %s", definition.name)
		}
		for _, option := range optionPattern.FindAllString(definition.usage, -1) {
			if !strings.Contains(body, option) {
				t.Errorf("%s help omitted %s", definition.name, option)
			}
		}
	}
}

func parserProbeArgs(name string) []string {
	switch name {
	case "help":
		return []string{"--help"}
	case "version":
		return []string{"--version"}
	}
	args := strings.Fields(name)
	switch name {
	case "documents show", "tasks show", "risks show":
		args = append(args, "1")
	case "resources list", "resources examples", "resources import":
		args = append(args, "assets")
	case "resources show", "resources pdf":
		args = append(args, "assets", "1")
	case "commands run":
		args = append(args, "assets.list")
	case "commands confirm", "commands reject":
		args = append(args, "synthetic-token")
	}
	return append(args, "--help")
}

func TestPublicDocumentationUsesTheCurrentCLISurface(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string]string{"README.md": string(readme), "skills/valiido/SKILL.md": skills.Valiido, "skills/valiido/references/importing.md": skills.ValiidoImporting}
	commands := regexp.MustCompile(`valiido[ \t]+([a-z][a-z_-]*)(?:[ \t]+([a-z][a-z_-]*))?`)
	flags := regexp.MustCompile(`--[a-z][a-z-]*`)
	for name, document := range documents {
		for _, match := range commands.FindAllStringSubmatch(document, -1) {
			noun := canonicalNoun(match[1])
			if match[2] == "" || noun == "setup" || noun == "open" && isResource(canonicalNoun(match[2])) ||
				noun == "help" && helpTopics[match[2]] != "" ||
				noun == "shell-completion" && (match[2] == "bash" || match[2] == "zsh" || match[2] == "fish") {
				continue
			}
			args := resourceShortcut([]string{match[1], match[2]})
			if _, ok := commandsByName[canonicalNoun(args[0])+" "+args[1]]; !ok {
				t.Errorf("%s documents unknown command %q", name, match[0])
			}
		}
		// Other tools in the documentation have their own flags. Check example
		// invocations of this CLI, including their continuation lines.
		for _, line := range strings.Split(strings.ReplaceAll(document, "\\\n", " "), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "valiido ") && !strings.HasPrefix(line, "./dist/valiido ") {
				continue
			}
			for _, option := range flags.FindAllString(line, -1) {
				if !documentedOption(option) {
					t.Errorf("%s documents unknown option %q", name, option)
				}
			}
		}
		lower := strings.ToLower(document)
		for _, internal := range []string{"/admin", "admin_", "00000000", "tenant_number", "database id", "system tenant", "/api/v1/tenants"} {
			if strings.Contains(lower, internal) {
				t.Errorf("%s exposes internal detail %q", name, internal)
			}
		}
	}
}

func TestGeneratedRoutesContainOnlyPublicAgentEndpoints(t *testing.T) {
	for key, path := range apiPaths {
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "{tenant_number}") {
			t.Errorf("invalid generated route %q: %q", key, path)
		}
		combined := strings.ToLower(key + " " + path)
		for _, internal := range []string{"admin", "/settings", "/connect", "tenant_number", "database"} {
			if strings.Contains(combined, internal) {
				t.Errorf("generated routes expose internal surface %q: %q", key, path)
			}
		}
	}
}

func documentedOption(option string) bool {
	if strings.Contains(help, option) {
		return true
	}
	for _, definition := range commandDefinitions {
		if definition.usage == "" {
			continue
		}
		for _, flag := range optionPattern.FindAllString(definition.usage, -1) {
			name, _, _ := strings.Cut(flag, " ")
			if name == option {
				return true
			}
		}
	}
	return false
}
