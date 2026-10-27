// Package valiido embeds the agent skill that `valiido setup` installs.
package valiido

import (
	"embed"
	"io/fs"
)

//go:embed skills/valiido
var skillFS embed.FS

//go:embed skills/valiido/SKILL.md
var Valiido string

//go:embed skills/valiido/references/importing.md
var ValiidoImporting string

// ValiidoFiles maps every installed skill file, relative to the skill directory, to its content.
// Go's embed skips names starting with "." or "_", so local virtualenvs and caches stay out.
var ValiidoFiles = func() map[string]string {
	root, err := fs.Sub(skillFS, "skills/valiido")
	if err != nil {
		panic(err)
	}
	files := map[string]string{}
	err = fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(root, path)
		files[path] = string(content)
		return err
	})
	if err != nil {
		panic(err)
	}
	return files
}()
