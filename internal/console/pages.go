package console

import (
	"embed"
	"html/template"
	"log"
	"net/http"
)

//go:embed templates/*.html
var templateFS embed.FS

// Templates are parsed once at init: a parse failure is a build defect, and
// discovering it on the first request instead of at startup would mean the
// login page is the thing that breaks.
var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

type loginData struct {
	Next  string
	Error string
}

type nodesData struct {
	Nodes  []nodeView
	User   string
	Prefix string
}

func renderNodes(w http.ResponseWriter, data nodesData) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	// Liveness is the point of this page; a cached copy would show a machine
	// as online long after it went away.
	w.Header().Set("cache-control", "no-store")
	if err := templates.ExecuteTemplate(w, "nodes.html", data); err != nil {
		log.Printf("console: cannot render machine list: %v", err)
	}
}

func renderLogin(w http.ResponseWriter, status int, next, message string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	// A login page must never be cached: a shared or restored browser would
	// otherwise redisplay it, or worse, a stale authenticated page behind it.
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	if err := templates.ExecuteTemplate(w, "login.html", loginData{Next: next, Error: message}); err != nil {
		// The status line is already written, so this can only be logged.
		log.Printf("console: cannot render login page: %v", err)
	}
}
