package embeddings

import "strings"

// fillTemplate substitutes {title} and {content} placeholders. Templates that
// omit {title} simply ignore the title argument.
func fillTemplate(tmpl, title, content string) string {
	r := strings.NewReplacer("{title}", title, "{content}", content)
	return r.Replace(tmpl)
}
