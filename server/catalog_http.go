package server

import (
	"net/http"
	"strings"

	stellarjay "github.com/kyle-visner/stellarjay"
)

func (a *API) getCatalog(w http.ResponseWriter, _ *http.Request) {
	if !a.catalogConfigured(w) {
		return
	}
	writeJSON(w, http.StatusOK, a.catalog.View())
}

func (a *API) installCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !a.catalogConfigured(w) {
		return
	}
	if !hasJSONContentType(r) {
		writeError(w, http.StatusUnsupportedMediaType, stellarjay.ErrValidation, "Content-Type must be application/json")
		return
	}
	var request CatalogEntry
	if err := decodeJSON(w, r, a.maxBody, &request); err != nil {
		writeDecodeError(w, err)
		return
	}
	entry, changed, err := a.catalog.Install(request.Type, request.Commands)
	if err != nil {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, err.Error())
		return
	}
	setRequestAudit(w, "catalog_install", "installed", "", 0)
	status := http.StatusOK
	if changed {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"entry": entry, "enforced": true})
}

func (a *API) removeCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !a.catalogConfigured(w) {
		return
	}
	eventType := strings.TrimSpace(r.PathValue("type"))
	if err := a.catalog.Remove(eventType); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "is not installed") {
			status = http.StatusNotFound
		}
		code := stellarjay.ErrValidation
		if status == http.StatusNotFound {
			code = stellarjay.ErrNotFound
		}
		writeError(w, status, code, err.Error())
		return
	}
	setRequestAudit(w, "catalog_remove", "removed", "", 0)
	view := a.catalog.View()
	writeJSON(w, http.StatusOK, map[string]any{"removed": eventType, "enforced": view.Enforced})
}

func (a *API) catalogConfigured(w http.ResponseWriter) bool {
	if a.catalog != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, stellarjay.ErrValidation, "catalog file is not configured")
	return false
}

func (a *API) authorizeAppend(w http.ResponseWriter, principal Principal, eventType, command string) bool {
	setAppendAudit(w, eventType, command)
	if err := principal.Allow.authorizeAppend(eventType, command); err != nil {
		writeError(w, http.StatusForbidden, stellarjay.ErrPermission, err.Error())
		return false
	}
	if a.catalog == nil {
		return true
	}
	if err := a.catalog.authorize(eventType, command); err != nil {
		writeError(w, http.StatusForbidden, stellarjay.ErrPermission, err.Error())
		return false
	}
	return true
}
