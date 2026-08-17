package console

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

//go:embed assets/overlay.js
var assetFS embed.FS

// OverlayScript is the tag injected into every page a machine serves.
//
// An external file rather than inline script: it survives a `script-src 'self'`
// that dsh may add later, and the browser caches it once per deploy instead of
// re-parsing it on every navigation.
var OverlayScript = `<script src="` + PathOverlay + `" defer></script>`

var (
	overlayBody []byte
	overlayETag string
)

func init() {
	body, err := assetFS.ReadFile("assets/overlay.js")
	if err != nil {
		// Embedded at build time; a failure here is a build defect.
		panic("console: cannot read overlay.js: " + err.Error())
	}
	overlayBody = body
	sum := sha256.Sum256(body)
	overlayETag = `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
}

// Overlay serves the injected console chrome.
//
// It sits behind the session guard like everything else under the prefix. An
// expired session therefore answers 401 and the script simply never loads,
// which is the right outcome: there is nothing for it to show.
func Overlay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/javascript; charset=utf-8")
	w.Header().Set("etag", overlayETag)
	// Revalidate every time but transfer nothing when unchanged. The file
	// changes only with a deploy, and a stale one would strand a browser with
	// no way back to the chooser.
	w.Header().Set("cache-control", "no-cache")

	if match := r.Header.Get("if-none-match"); match != "" && strings.Contains(match, overlayETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(w, r, "overlay.js", time.Time{}, strings.NewReader(string(overlayBody)))
}
