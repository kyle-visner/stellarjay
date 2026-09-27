package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed website/index.html website/llm.txt
var publicSite embed.FS

func (a *API) siteHome(w http.ResponseWriter, _ *http.Request) {
	writeSiteFile(w, "website/index.html", "text/html; charset=utf-8")
}

func (a *API) siteLLM(w http.ResponseWriter, _ *http.Request) {
	writeSiteFile(w, "website/llm.txt", "text/plain; charset=utf-8")
}

func writeSiteFile(w http.ResponseWriter, name, contentType string) {
	raw, err := fs.ReadFile(publicSite, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
