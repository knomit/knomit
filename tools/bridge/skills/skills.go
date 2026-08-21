// Package skills embeds the knomit skill templates shared by every agent host.
//
// It exists as a package rather than a bare directory because //go:embed
// patterns cannot contain "..", so sibling packages (claude, antigravity)
// cannot embed a shared directory directly — they consume this FS instead.
package skills

import "embed"

// Root is the directory inside FS holding one subdirectory per skill.
const Root = "templates"

//go:embed all:templates
var FS embed.FS
