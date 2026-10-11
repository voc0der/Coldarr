package webui

import (
	"embed"
	"fmt"
	"html/template"

	"github.com/vocoder/coldarr/internal/model"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pageNames lists every top-level page, each rendered by combining a layout
// (layout.html, or portal_layout.html for the nav-less login page) with
// templates/<name>.html and the shared partials. Each
// entry is parsed into its own *template.Template so that every page's
// {{define "content"}} is isolated - html/template names are global
// within one Template, so parsing all pages together would let the last
// one parsed silently win for every page.
var pageNames = []string{
	"dashboard",
	"connections",
	"tiers",
	"tier_form",
	"settings_auth",
	"settings_notifications",
	"settings_scheduler",
	"settings_orphans",
	"settings_downloads",
	"plan",
	"downloads",
	"history",
	"verify_status",
	"login",
}

// navState says which parts of the GUI apply to this install: library
// tiering, download tiering, or both. Pages for the part that doesn't apply
// stay out of sight, so a library-only setup looks exactly as it did before
// downloads tiering existed.
type navState struct {
	// Library shows the library pages - Plan and History.
	Library bool
	// Downloads shows the downloads page and settings.
	Downloads bool
}

// parseTemplates parses every page with a nav that shows only the library
// pages - what tests that build a Server by hand get.
func parseTemplates() (map[string]*template.Template, error) {
	return parseTemplatesWith(func() navState { return navState{Library: true} })
}

// parseTemplatesWith parses every page, with nav answering what the layout
// and settings pages ask about this install.
func parseTemplatesWith(nav func() navState) (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"nav": nav,
		"gb": func(bytes int64) string {
			return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
		},
		"pct": func(f float64) string {
			return fmt.Sprintf("%.1f%%", f)
		},
		"defaultHotMax": func() float64 {
			return model.DefaultHotMaxUsedPercent
		},
	}

	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New("root").Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html",
			"templates/portal_layout.html",
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
