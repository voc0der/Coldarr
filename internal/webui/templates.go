package webui

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pageNames lists every top-level page, each rendered by combining
// layout.html with templates/<name>.html and the shared partials. Each
// entry is parsed into its own *template.Template so that every page's
// {{define "content"}} is isolated - html/template names are global
// within one Template, so parsing all pages together would let the last
// one parsed silently win for every page.
var pageNames = []string{
	"dashboard",
	"connections",
	"tiers",
	"tier_form",
	"plan",
	"apply_result",
	"history",
}

func parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"gb": func(bytes int64) string {
			return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
		},
		"pct": func(f float64) string {
			return fmt.Sprintf("%.1f%%", f)
		},
	}

	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New("root").Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html",
			"templates/"+name+".html",
			"templates/partials/*.html",
		)
		if err != nil {
			return nil, fmt.Errorf("parsing templates for page %q: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}
