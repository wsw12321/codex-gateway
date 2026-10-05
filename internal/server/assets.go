package server

import (
	_ "embed"
	"net/http"
)

//go:embed assets/index.html
var indexHTML []byte

//go:embed assets/app.js
var appJS []byte

//go:embed assets/style.css
var styleCSS []byte

//go:embed assets/theme.js
var themeJS []byte

//go:embed assets/favicon.svg
var faviconSVG []byte

//go:embed assets/configure-client.cjs
var clientSetupScript []byte

func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

func (s *Server) javascript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(appJS)
}

func (s *Server) stylesheet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(styleCSS)
}

func (s *Server) themeJavascript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(themeJS)
}

func (s *Server) favicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(faviconSVG)
}

//go:embed assets/oidc-callback.html
var oidcCallbackHTML []byte

//go:embed assets/oidc-callback.js
var oidcCallbackJS []byte

// A cross-site GET intentionally does not inspect cookies, redeem codes or
// mutate identity. The landing page performs a subsequent same-origin POST.
func (s *Server) oidcCallback(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(oidcCallbackHTML)
}
func (s *Server) oidcCallbackJavascript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(oidcCallbackJS)
}
