package web

import (
	"log"
	"net/http"
)

func (s *Server) getTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		apiError(w, http.StatusNotFound, "tunnel manager not available")
		return
	}
	st := s.tunnel.Status()
	if st.TunnelID == "" {
		apiError(w, http.StatusNotFound, "no tunnel configured")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		errorResponse(w, http.StatusServiceUnavailable, "tunnel manager not available")
		return
	}
	if st := s.tunnel.Status(); st.TunnelID != "" {
		errorResponse(w, http.StatusConflict, "tunnel already exists")
		return
	}
	if _, err := s.tunnel.Create(r.Context()); err != nil {
		log.Printf("web: create tunnel: %v", err)
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	renderTemplate(w, "tunnel.html", s.buildTunnelFragData())
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		errorResponse(w, http.StatusNotFound, "tunnel manager not available")
		return
	}
	if err := s.tunnel.Delete(r.Context()); err != nil {
		log.Printf("web: delete tunnel: %v", err)
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	renderTemplate(w, "tunnel.html", s.buildTunnelFragData())
}
