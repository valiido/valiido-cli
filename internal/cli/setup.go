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
	if err := rejectDuplicateFlags(args, flags); err != nil {
		return err
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprint(stdout, help)
			return err
		}
		return failure("usage", "Invalid setup options. Run valiido --help.")
	}
	if flags.NArg() != 0 || (*agent != "all" && *agent != "codex" && *agent != "claude") {
		return failure("usage", "Use "+commandUsage("setup")+".")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return failure("storage", "Cannot find the user home directory.")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return failure("storage", "Cannot access the user home directory.")
	}
	homeInfo, err := os.Stat(home)
	if err != nil || !homeInfo.IsDir() || homeInfo.Mode().Perm()&0022 != 0 {
		return failure("storage", "The user home directory has unsafe permissions.")
	}
	paths := make([]string, 0, 2)
	for _, target := range []struct{ name, directory string }{{"codex", ".agents"}, {"claude", ".claude"}} {
		if *agent != "all" && *agent != target.name {
			continue
		}
		path, err := secureSkillPath(home, target.directory)
		if err != nil {
			return err
		}
		paths = append(paths, path)
	}
	// Check every target before writing any skill, so `setup --agent all` does
	// not leave a partial installation after a known conflict.
	for _, path := range paths {
		if err := checkSkill(path, *force); err != nil {
			return err
		}
	}
	for _, path := range paths {
		if err := installSkill(path, *force); err != nil {
			return err
		}
	}
	return success(stdout, map[string]any{"skills": paths})
}

func secureSkillPath(home, agentDirectory string) (string, error) {
	current := home
	for _, component := range []string{agentDirectory, "skills", "valiido"} {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0755); err != nil {
				return "", failure("storage", "Cannot create the Valiido skill directory.")
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return "", failure("storage", "Valiido skill directories must be private from untrusted writers and cannot be symlinks.")
		}
	}
	return filepath.Join(current, "SKILL.md"), nil
}

func checkSkill(path string, force bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return failure("storage", "Cannot safely replace the existing Valiido skill.")
	}
	content, readErr := readBoundedFile(path, int64(len(skills.Valiido)))
	if readErr == nil && string(content) == skills.Valiido {
		return nil
	}
	if !force {
		return failure("storage", "A different Valiido skill already exists. Move it aside or use --force.")
	}
	return nil
}

func installSkill(path string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return failure("storage", "Cannot create the Valiido skill directory.")
	}
	if err := checkSkill(path, force); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if errors.Is(err, os.ErrExist) {
		content, readErr := readBoundedFile(path, int64(len(skills.Valiido)))
		if readErr == nil && string(content) == skills.Valiido {
			if os.Chmod(path, 0644) != nil {
				return failure("storage", "Cannot secure the installed Valiido skill.")
			}
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
