package server

import (
	"net/http"
	"strings"
)

const codexGatewayBaseURLPlaceholder = "__CODEX_GATEWAY_BASE_URL__"

func (s *Server) codexShellSetup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(renderCodexSetupScript(codexShellSetupScript, s.codexGatewayBaseURL()))
}

func (s *Server) codexWindowsSetup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-msdos-program")
	w.Header().Set("Content-Disposition", `attachment; filename="configure-codex.bat"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(renderCodexSetupScript(codexWindowsSetupScript, s.codexGatewayBaseURL()))
}

func (s *Server) codexGatewayBaseURL() string {
	return s.config.PublicURL.Scheme + "://" + s.config.PublicURL.Host + "/v1"
}

func renderCodexSetupScript(template []byte, baseURL string) []byte {
	return []byte(strings.ReplaceAll(string(template), codexGatewayBaseURLPlaceholder, baseURL))
}
