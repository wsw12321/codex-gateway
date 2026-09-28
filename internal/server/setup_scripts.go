package server

import "net/http"

// The launcher supplies the origin visible in the browser, which can differ
// from PublicURL when the same deployment has multiple entry points.
func (s *Server) clientSetup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(clientSetupScript)
}

func (s *Server) retiredClientSetup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "This setup script has been retired. Open the gateway usage guide for the current client configuration command.", http.StatusGone)
}
