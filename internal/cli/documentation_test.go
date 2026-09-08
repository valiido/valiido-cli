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
		if definition.name == "" || definition.usage == "" || seen[definition.name] {
			t.Fatalf("invalid or duplicate command definition: %#v", definition)
		}
		seen[definition.name] = true
		if !strings.Contains(help, "  "+definition.usage+"\n") {
			t.Fatalf("help omitted %q", definition.name)
		}
	}
	if len(seen) != len(knownCommands) {
		t.Fatal("known command index drifted from command definitions")
	}
}

func TestEveryDocumentedFixedCommandIsAcceptedByTheParser(t *testing.T) {
	for _, definition := range commandDefinitions {
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

func TestEveryDocumentedOptionBelongsToItsCommand(t *testing.T) {
	options := regexp.MustCompile(`--[a-z][a-z-]*`)
	values := map[string]string{
		"--agent": "codex", "--profile": "work", "--server": "http://127.0.0.1:3000",
		"--query": "text", "--limit": "1", "--file": "synthetic.json",
		"--output": "synthetic.pdf", "--input": `{}`,
	}
	for _, definition := range commandDefinitions {
		if definition.name == "help" || definition.name == "version" {
			continue
		}
		for _, option := range options.FindAllString(definition.usage, -1) {
			args := parserProbeArgs(definition.name)
			args = args[:len(args)-1]
			args = append(args, option)
			if value := values[option]; value != "" {
				args = append(args, value)
			}
			args = append(args, "--help")
			var output strings.Builder
			expected, _ := contextualHelp(args)
			if code := Run(t.Context(), args, &output, &output); code != 0 || output.String() != expected {
				t.Errorf("%s advertises unsupported option %s", definition.name, option)
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
	case "documents read", "tasks read", "risks read":
		args = append(args, "1")
	case "resources list", "resources examples", "resources import":
		args = append(args, "assets")
	case "resources read", "resources pdf":
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
	documents := map[string]string{"README.md": string(readme), "skills/valiido/SKILL.md": skills.Valiido}
	commands := regexp.MustCompile(`valiido[ \t]+([a-z]+)(?:[ \t]+([a-z]+))?`)
	flags := regexp.MustCompile(`--[a-z][a-z-]*`)
	for name, document := range documents {
		for _, match := range commands.FindAllStringSubmatch(document, -1) {
			noun := canonicalNoun(match[1])
			if noun == "setup" || match[2] == "" {
				continue
			}
			if !knownCommands[noun+" "+match[2]] {
				t.Errorf("%s documents unknown command %q", name, match[0])
			}
		}
		for _, option := range flags.FindAllString(document, -1) {
			if !strings.Contains(help, option) {
				t.Errorf("%s documents unknown option %q", name, option)
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
