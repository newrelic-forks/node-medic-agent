package render

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

// templatesFS bundles the on-call UI's HTML templates into the binary
// at build time. Mirrored under internal/oncall/render/templates/.
//
//go:embed templates/*.html.tmpl
var templatesFS embed.FS

// staticFS bundles CSS + JS into the binary. Served via
// http.FileServer at /static/.
//
//go:embed static/*
var staticFS embed.FS

// templateFuncs are the helpers the layout/list/detail templates lean
// on. Kept small — most formatting work happens in data.go (the
// composers) so the templates stay close to data-binding only.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format("2006-01-02T15:04:05Z")
		},
		"formatRelative": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"formatConfidence": func(c float64) string {
			if c < 0 {
				return "—"
			}
			return fmt.Sprintf("%.2f", c)
		},
		"truncRef": func(s string) string {
			const maxLen = 120
			if len(s) <= maxLen {
				return s
			}
			return s[:maxLen-1] + "…"
		},
	}
}

// pageTemplates wires one template tree per page. Each tree carries
// the layout + that page's overrides for title/content/scripts. Built
// per-page rather than as a single tree because list.html.tmpl and
// detail.html.tmpl both `{{define "content"}}` and the last-parsed
// definition would clobber the first in a shared tree.
//
// Shared partials (layout, _action_history) live in their own files
// and are parsed into every page tree.
var (
	pagesOnce sync.Once
	pages     map[string]*template.Template
	pagesErr  error
)

// sharedTemplateNames lists the templates every page tree needs.
var sharedTemplateNames = []string{"layout.html.tmpl", "_action_history.html.tmpl"}

// pageTemplateNames lists the per-page templates. Each name maps to
// a separate parsed tree.
var pageTemplateNames = []string{"list.html.tmpl", "detail.html.tmpl"}

func loadPages() (map[string]*template.Template, error) {
	pagesOnce.Do(func() {
		shared := make(map[string][]byte, len(sharedTemplateNames))
		for _, name := range sharedTemplateNames {
			b, err := fs.ReadFile(templatesFS, "templates/"+name)
			if err != nil {
				pagesErr = fmt.Errorf("read shared template %s: %w", name, err)
				return
			}
			shared[name] = b
		}

		pages = make(map[string]*template.Template, len(pageTemplateNames))
		for _, name := range pageTemplateNames {
			b, err := fs.ReadFile(templatesFS, "templates/"+name)
			if err != nil {
				pagesErr = fmt.Errorf("read page template %s: %w", name, err)
				return
			}
			// Seed the tree with the first shared template so the
			// receiver template's tree is non-nil before we add
			// siblings. (`template.New(name).Parse(content)` is the
			// canonical pattern; `t.New(...)` returns associated
			// templates that share the same parse-tree namespace.)
			var t *template.Template
			seeded := false
			for sname, sb := range shared {
				if !seeded {
					var err error
					t, err = template.New(sname).Funcs(templateFuncs()).Parse(string(sb))
					if err != nil {
						pagesErr = fmt.Errorf("parse shared %s: %w", sname, err)
						return
					}
					seeded = true
					continue
				}
				if _, err := t.New(sname).Parse(string(sb)); err != nil {
					pagesErr = fmt.Errorf("parse shared %s: %w", sname, err)
					return
				}
			}
			if t == nil {
				// no shared templates configured; fall back to a
				// fresh tree
				t = template.New(name).Funcs(templateFuncs())
			}
			if _, err := t.New(name).Parse(string(b)); err != nil {
				pagesErr = fmt.Errorf("parse page %s: %w", name, err)
				return
			}
			pages[name] = t
		}
	})
	return pages, pagesErr
}

// PageTemplate returns the parsed *template.Template for one page.
// Exposed for tests that want to ExecuteTemplate directly.
func PageTemplate(name string) (*template.Template, error) {
	ps, err := loadPages()
	if err != nil {
		return nil, err
	}
	t := ps[name]
	if t == nil {
		return nil, fmt.Errorf("render: no page template named %q", name)
	}
	return t, nil
}

// Suppress the unused-import warning surfaced when this file is
// edited without strings.HasSuffix below.
var _ = strings.HasSuffix

// StaticFS returns the embedded static-asset FS rooted at "static".
// http.FileServer(http.FS(StaticFS())) serves /static/* paths.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed directive guarantees the directory exists; this is
		// a build-time invariant.
		panic(fmt.Sprintf("render: static FS sub: %v", err))
	}
	return sub
}

// RenderList writes the list page HTML to w. Returns the same error
// shape as ExecuteTemplate so middleware can decide what to log.
func RenderList(w io.Writer, rows []ListPageRow, errBanner string) error {
	t, err := PageTemplate("list.html.tmpl")
	if err != nil {
		return err
	}
	return t.ExecuteTemplate(w, "list.html.tmpl", listTemplateData{
		Rows:        rows,
		ErrorBanner: errBanner,
		GeneratedAt: time.Now().UTC(),
	})
}

// RenderDetail writes the per-case page HTML to w.
func RenderDetail(w io.Writer, data DetailPageData, errBanner string, notFound bool) error {
	t, err := PageTemplate("detail.html.tmpl")
	if err != nil {
		return err
	}
	return t.ExecuteTemplate(w, "detail.html.tmpl", detailTemplateData{
		Page:        data,
		ErrorBanner: errBanner,
		NotFound:    notFound,
	})
}

// listTemplateData wraps the slice plus chrome inputs the list
// template uses (error banner, generated-at footer).
type listTemplateData struct {
	Rows        []ListPageRow
	ErrorBanner string
	GeneratedAt time.Time
}

type detailTemplateData struct {
	Page        DetailPageData
	ErrorBanner string
	NotFound    bool
}

// ContentTypeHTML is the response Content-Type the handlers set.
const ContentTypeHTML = "text/html; charset=utf-8"

// WriteHTMLHeader sets the standard html response headers; handlers
// call this before ExecuteTemplate.
func WriteHTMLHeader(h http.Header) {
	h.Set("Content-Type", ContentTypeHTML)
}
