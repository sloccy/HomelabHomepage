package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"lantern/internal/store"
	"lantern/internal/tunnel"
)

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

//go:embed templates
var templateFS embed.FS

var tmpl *template.Template
var indexTmpl *template.Template
var manageTmpl *template.Template

func init() {
	entries, err := fs.ReadDir(templateFS, "templates/partials")
	if err != nil {
		log.Fatalf("web: read templates: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			files = append(files, "templates/partials/"+e.Name())
		}
	}
	tmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templateFS, files...))
	indexTmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templateFS,
		"templates/base.html", "templates/index.html"))
	manageTmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templateFS,
		"templates/base.html", "templates/manage.html"))
}

func renderTemplate(w http.ResponseWriter, name string, data any) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := tmpl.ExecuteTemplate(buf, name, data); err != nil {
		log.Printf("web: render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

type pageData struct {
	Version      string
	ServicesHTML  template.HTML
	BookmarksHTML template.HTML
}

func renderPage(w http.ResponseWriter, t *template.Template, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("web: render page: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func preRender(name string, data any) template.HTML {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	_ = tmpl.ExecuteTemplate(buf, name, data)
	return template.HTML(buf.String())
}

// hxTrigger writes one or more HX-Trigger event names as a JSON header.
// Pass alternating key, value pairs; a nil value becomes JSON null.
//
//	hxTrigger(w, "closeModal", nil, "showToast", map[string]string{...})
func hxTrigger(w http.ResponseWriter, kvs ...any) {
	if len(kvs) == 0 {
		return
	}
	m := make(map[string]any, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		m[fmt.Sprintf("%v", kvs[i])] = kvs[i+1]
	}
	b, _ := json.Marshal(m)
	w.Header().Set("HX-Trigger", string(b))
}

// toastTrigger writes HX-Trigger headers that close the modal and show a toast.
func toastTrigger(w http.ResponseWriter, msg, typ string, extraEvents ...string) {
	m := map[string]any{
		"closemodal": nil,
		"showtoast":  map[string]string{"msg": msg, "type": typ},
	}
	for _, ev := range extraEvents {
		m[ev] = nil
	}
	b, _ := json.Marshal(m)
	w.Header().Set("HX-Trigger", string(b))
}

// errorTrigger writes HX-Trigger for an error toast (no modal close).
func errorTrigger(w http.ResponseWriter, msg string) {
	b, _ := json.Marshal(map[string]any{
		"showtoast": map[string]string{"msg": msg, "type": "error"},
	})
	w.Header().Set("HX-Trigger", string(b))
}

// ---- Template function map --------------------------------------------------

var funcMap = template.FuncMap{
	// iconEl renders the icon HTML for a service/bookmark.
	// id: entity ID; icon: "file", emoji, or empty; src: URL for favicon proxy; cls: CSS class.
	"iconEl": func(id, icon, src, cls string) template.HTML {
		if icon == "file" {
			return template.HTML(fmt.Sprintf(
				`<img class="%s" src="/api/icons/%s" alt="">`,
				template.HTMLEscapeString(cls),
				template.HTMLEscapeString(id),
			))
		}
		// Emoji icon from fingerprint.
		if icon != "" && !strings.HasPrefix(icon, "data:") {
			ph := strings.TrimSuffix(cls, "-icon") + "-icon-placeholder"
			return template.HTML(fmt.Sprintf(`<div class="%s">%s</div>`,
				template.HTMLEscapeString(ph), template.HTMLEscapeString(icon)))
		}
		// Legacy: handle old data URIs that weren't migrated (e.g., fresh in-memory only).
		if strings.HasPrefix(icon, "data:") {
			return template.HTML(fmt.Sprintf(
				`<img class="%s" src="%s" alt="">`,
				template.HTMLEscapeString(cls),
				template.HTMLEscapeString(icon),
			))
		}
		if src != "" {
			proxyURL := "/api/favicon?url=" + url.QueryEscape(src)
			ph := strings.TrimSuffix(cls, "-icon") + "-icon-placeholder"
			return template.HTML(fmt.Sprintf(
				`<img class="%s" src="%s" alt="" onerror="this.outerHTML='<div class=&quot;%s&quot;>📦</div>'">`,
				template.HTMLEscapeString(cls),
				template.HTMLEscapeString(proxyURL),
				template.HTMLEscapeString(ph),
			))
		}
		ph := strings.TrimSuffix(cls, "-icon") + "-icon-placeholder"
		return template.HTML(fmt.Sprintf(`<div class="%s">📦</div>`, template.HTMLEscapeString(ph)))
	},

	// faviconURL builds the favicon proxy URL.
	"faviconURL": func(target string) string {
		return "/api/favicon?url=" + url.QueryEscape(target)
	},

	// serviceURL builds https://sub.domain
	"serviceURL": func(subdomain, domain string) string {
		return "https://" + subdomain + "." + domain
	},

	// cardURL returns the URL a service card should link to.
	// DirectOnly services link straight to their target; others use subdomain.domain.
	"cardURL": func(svc *store.Service, domain string) string {
		if svc.DirectOnly {
			return svc.Target
		}
		return "https://" + svc.Subdomain + "." + domain
	},

	// tagClass returns CSS classes for a source badge.
	"tagClass": func(source string) string {
		switch source {
		case "docker":
			return "badge text-bg-info"
		case "network":
			return "badge text-bg-success"
		default:
			return "badge text-bg-secondary"
		}
	},

	// isTunneled reports if a service is routed via CF Tunnel.
	"isTunneled": func(svc *store.Service) bool {
		return svc.TunnelRouteID != ""
	},

	// healthStatus returns the health string for a service ID.
	"healthStatus": func(id string, health map[string]string) string {
		if health == nil {
			return ""
		}
		return health[id]
	},

	// fmtTime formats a time.Time for display.
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("Jan 2, 2006 15:04")
	},

	// logClass returns the CSS class for a scan log line.
	"logClass": func(line string) string {
		switch {
		case strings.Contains(line, "[OPEN]"):
			return "scan-log-line open"
		case strings.Contains(line, "[TCP]"):
			return "scan-log-line tcp"
		case strings.Contains(line, "[HTTP]"):
			return "scan-log-line http"
		case strings.Contains(line, "[ARP]"):
			return "scan-log-line arp"
		case strings.Contains(line, "[SCAN]"):
			return "scan-log-line scan"
		case strings.Contains(line, "[ERR]"):
			return "scan-log-line err"
		default:
			return "scan-log-line"
		}
	},

	// qesc URL-encodes a string for use in query params.
	"qesc": url.QueryEscape,

	// confPct converts a float32 confidence to an integer percentage.
	"confPct": func(f float32) int { return int(f * 100) },

	// fmtMem formats MB as "X.X GB" or "X MB".
	"fmtMem": func(mb uint64) string {
		if mb >= 1024 {
			return fmt.Sprintf("%.1f GB", float64(mb)/1024)
		}
		return fmt.Sprintf("%d MB", mb)
	},

	// isDataIcon reports whether the icon string is a data URI (legacy).
	"isDataIcon": func(icon string) bool {
		return strings.HasPrefix(icon, "data:")
	},

	// isFileIcon reports whether the icon is stored as a file on disk.
	"isFileIcon": func(icon string) bool {
		return icon == "file"
	},

	// safeURL returns a template.URL to bypass Go's URL sanitisation for
	// trusted internal URLs (data URIs, favicon proxy paths).
	"safeURL": func(s string) template.URL {
		return template.URL(s) //nolint:gosec
	},
}

// ---- Template data types ----------------------------------------------------

type serviceGroup struct {
	Name     string
	Services []*store.Service
}

type servicesGridData struct {
	Groups    []serviceGroup
	Domain    string
	Health    map[string]string
	IsEmpty   bool
	Searching bool
}

type bookmarkGroup struct {
	Name      string
	Bookmarks []*store.Bookmark
}

type bookmarksGridData struct {
	Groups []bookmarkGroup
}

type tunnelFragData struct {
	Status    tunnel.TunnelStatus
	Available bool
}

type servicesTableData struct {
	Services []*store.Service
	Domain   string
}

type serviceFormData struct {
	Service       *store.Service // nil = add new
	Categories    []string
	Domain        string
	TunnelEnabled bool
}

type assignFormData struct {
	Discovered   *store.DiscoveredService
	Categories   []string
	Domain       string
	TunnelEnabled bool
}

type bookmarkFormData struct {
	Bookmark   *store.Bookmark // nil = add new
	Categories []string
}

type ddnsFragData struct {
	Domains  []string
	PublicIP string
}

type subnetsFragData struct {
	Subnets []string
}

// ---- Helper functions -------------------------------------------------------

// sortAndGroup sorts items by Order then Name, groups them by Category, and
// returns ordered category keys (empty category first, then alphabetical) plus
// a map from category name to its items.
func sortAndGroup[T any](items []T, order func(T) int, name func(T) string, category func(T) string) ([]string, map[string][]T) {
	sorted := make([]T, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		if order(sorted[i]) != order(sorted[j]) {
			return order(sorted[i]) < order(sorted[j])
		}
		return strings.ToLower(name(sorted[i])) < strings.ToLower(name(sorted[j]))
	})

	groups := make(map[string][]T)
	var keys []string
	seen := make(map[string]bool)
	for _, item := range sorted {
		cat := category(item)
		if !seen[cat] {
			seen[cat] = true
			keys = append(keys, cat)
		}
		groups[cat] = append(groups[cat], item)
	}

	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i] == "" {
			return true
		}
		if keys[j] == "" {
			return false
		}
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	return keys, groups
}

func buildServicesGrid(services []*store.Service, domain string, health map[string]string, searching bool) servicesGridData {
	keys, groups := sortAndGroup(services,
		func(s *store.Service) int { return s.Order },
		func(s *store.Service) string { return s.Name },
		func(s *store.Service) string { return s.Category },
	)
	svcGroups := make([]serviceGroup, len(keys))
	for i, k := range keys {
		svcGroups[i] = serviceGroup{Name: k, Services: groups[k]}
	}
	return servicesGridData{
		Groups:    svcGroups,
		Domain:    domain,
		Health:    health,
		IsEmpty:   len(services) == 0,
		Searching: searching,
	}
}

func buildBookmarksGrid(bookmarks []*store.Bookmark) bookmarksGridData {
	keys, groups := sortAndGroup(bookmarks,
		func(b *store.Bookmark) int { return b.Order },
		func(b *store.Bookmark) string { return b.Name },
		func(b *store.Bookmark) string { return b.Category },
	)
	bmGroups := make([]bookmarkGroup, len(keys))
	for i, k := range keys {
		bmGroups[i] = bookmarkGroup{Name: k, Bookmarks: groups[k]}
	}
	return bookmarksGridData{Groups: bmGroups}
}

func getUniqueCategories(services []*store.Service, bookmarks []*store.Bookmark) []string {
	seen := make(map[string]bool)
	var cats []string
	for _, svc := range services {
		if svc.Category != "" && !seen[svc.Category] {
			seen[svc.Category] = true
			cats = append(cats, svc.Category)
		}
	}
	for _, bm := range bookmarks {
		if bm.Category != "" && !seen[bm.Category] {
			seen[bm.Category] = true
			cats = append(cats, bm.Category)
		}
	}
	sort.Strings(cats)
	return cats
}
