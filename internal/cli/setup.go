package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/valiido/valiido-cli/skills"
)

func setup(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	agent := flags.String("agent", "all", "Install for codex, claude or all")
	force := flags.Bool("force", false, "Replace an existing Valiido skill")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprint(stdout, help)
			return err
		}
		return failure("usage", "Invalid setup options. Run valiido --help.")
	}
	if flags.NArg() != 0 || (*agent != "all" && *agent != "codex" && *agent != "claude") {
		return failure("usage", "Use valiido setup [--agent codex|claude|all] [--force].")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return failure("storage", "Cannot find the user home directory.")
	}
	installed := []string{}
	for _, target := range []struct{ name, directory string }{{"codex", ".agents"}, {"claude", ".claude"}} {
		if *agent != "all" && *agent != target.name {
			continue
		}
		path := filepath.Join(home, target.directory, "skills", "valiido", "SKILL.md")
		if err := installSkill(path, *force); err != nil {
			return err
		}
		installed = append(installed, path)
	}
	return success(stdout, map[string]any{"skills": installed})
}

func installSkill(path string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return failure("storage", "Cannot create the Valiido skill directory.")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if errors.Is(err, os.ErrExist) {
		content, readErr := os.ReadFile(path)
		if readErr == nil && string(content) == skills.Valiido {
			return nil
		}
		if force {
			info, statErr := os.Lstat(path)
			if statErr != nil || !info.Mode().IsRegular() || os.Remove(path) != nil {
				return failure("storage", "Cannot safely replace the existing Valiido skill.")
			}
			return installSkill(path, false)
		}
		return failure("storage", "A different Valiido skill already exists. Move it aside before running setup again.")
	}
	if err != nil {
		return failure("storage", "Cannot create the Valiido skill file.")
	}
	_, writeErr := io.WriteString(file, skills.Valiido)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return failure("storage", "Cannot finish writing the Valiido skill file.")
	}
	return nil
}
