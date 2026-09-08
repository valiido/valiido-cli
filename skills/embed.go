package skills

import _ "embed"

//go:embed valiido/SKILL.md
var Valiido string

//go:embed valiido/references/importing.md
var ValiidoImporting string

var ValiidoFiles = map[string]string{
	"SKILL.md":                Valiido,
	"references/importing.md": ValiidoImporting,
}
