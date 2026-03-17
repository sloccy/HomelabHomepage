package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"lantern/internal/store"
	"lantern/internal/tunnel"
)

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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("web: render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func renderPage(w http.ResponseWriter, t *template.Template) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", nil); err != nil {
		log.Printf("web: render page: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// hxTrigger writes one or more HX-Trigger event names as a JSON header.
// Pass alternating key, value pairs; a nil value becomes JSON null.
//
//	hxTrigger(w, "closeModal", nil, "showToast", map[string]string{...})
func hxTrigger(w http.ResponseWriter, kvs ...any) {
	if len(kvs) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i+1 < len(kvs); i += 2 {
		if i > 0 {
			sb.WriteString(",")
		}
		key := fmt.Sprintf("%v", kvs[i])
		sb.WriteString(`"` + key + `":`)
		val := kvs[i+1]
		if val == nil {
			sb.WriteString("null")
		} else {
			switch v := val.(type) {
			case map[string]string:
				sb.WriteString("{")
				first := true
				for mk, mv := range v {
					if !first {
						sb.WriteString(",")
					}
					sb.WriteString(fmt.Sprintf(`"%s":"%s"`, mk, mv))
					first = false
				}
				sb.WriteString("}")
			default:
				sb.WriteString(fmt.Sprintf(`"%v"`, v))
			}
		}
	}
	sb.WriteString("}")
	w.Header().Set("HX-Trigger", sb.String())
}

// toastTrigger writes HX-Trigger headers that close the modal and show a toast.
func toastTrigger(w http.ResponseWriter, msg, typ string, extraEvents ...string) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"closemodal":null,"showtoast":{"msg":%q,"type":%q}`, msg, typ))
	for _, ev := range extraEvents {
		sb.WriteString(fmt.Sprintf(`,%q:null`, ev))
	}
	sb.WriteString("}")
	w.Header().Set("HX-Trigger", sb.String())
}

// errorTrigger writes HX-Trigger for an error toast (no modal close).
func errorTrigger(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Trigger", fmt.Sprintf(`{"showtoast":{"msg":%q,"type":"error"}}`, msg))
}

// ---- Template function map --------------------------------------------------

var funcMap = template.FuncMap{
	// iconEl renders the icon HTML for a service/bookmark.
	// icon: data URI or empty; src: URL for favicon proxy; cls: CSS class.
	"iconEl": func(icon, src, cls string) template.HTML {
		if strings.HasPrefix(icon, "data:") {
			return template.HTML(fmt.Sprintf(
				`<img class="%s" src="%s" alt="">`,
				template.HTMLEscapeString(cls),
				template.HTMLEscapeString(icon),
			))
		}
		if src != "" {
			proxyURL := "/api/favicon?url=" + url.QueryEscape(src)
			return template.HTML(fmt.Sprintf(
				`<img class="%s" src="%s" alt="" onerror="this.style.display='none'">`,
				template.HTMLEscapeString(cls),
				template.HTMLEscapeString(proxyURL),
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

	// tagClass returns CSS classes for a source tag.
	"tagClass": func(source string) string {
		switch source {
		case "docker":
			return "badge badge-info"
		case "network":
			return "badge badge-success"
		default:
			return "badge badge-ghost"
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

	// isDataIcon reports whether the icon string is a data URI.
	"isDataIcon": func(icon string) bool {
		return strings.HasPrefix(icon, "data:")
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

func buildServicesGrid(services []*store.Service, domain string, health map[string]string, searching bool) servicesGridData {
	sorted := make([]*store.Service, len(services))
	copy(sorted, services)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Order != sorted[j].Order {
			return sorted[i].Order < sorted[j].Order
		}
		return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name)
	})

	groupIdx := make(map[string]int)
	var groups []serviceGroup
	for _, svc := range sorted {
		cat := svc.Category
		if _, ok := groupIdx[cat]; !ok {
			groupIdx[cat] = len(groups)
			groups = append(groups, serviceGroup{Name: cat})
		}
		i := groupIdx[cat]
		groups[i].Services = append(groups[i].Services, svc)
	}

	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Name == "" {
			return true
		}
		if groups[j].Name == "" {
			return false
		}
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})

	return servicesGridData{
		Groups:    groups,
		Domain:    domain,
		Health:    health,
		IsEmpty:   len(services) == 0,
		Searching: searching,
	}
}

func buildBookmarksGrid(bookmarks []*store.Bookmark) bookmarksGridData {
	sorted := make([]*store.Bookmark, len(bookmarks))
	copy(sorted, bookmarks)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Order != sorted[j].Order {
			return sorted[i].Order < sorted[j].Order
		}
		return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name)
	})

	groupIdx := make(map[string]int)
	var groups []bookmarkGroup
	for _, bm := range sorted {
		cat := bm.Category
		if _, ok := groupIdx[cat]; !ok {
			groupIdx[cat] = len(groups)
			groups = append(groups, bookmarkGroup{Name: cat})
		}
		i := groupIdx[cat]
		groups[i].Bookmarks = append(groups[i].Bookmarks, bm)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Name == "" {
			return true
		}
		if groups[j].Name == "" {
			return false
		}
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	return bookmarksGridData{Groups: groups}
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
