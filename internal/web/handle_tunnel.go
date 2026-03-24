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
		errorTrigger(w, "tunnel manager not available")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if st := s.tunnel.Status(); st.TunnelID != "" {
		errorTrigger(w, "tunnel already exists")
		w.WriteHeader(http.StatusConflict)
		return
	}
	if _, err := s.tunnel.Create(r.Context()); err != nil {
		log.Printf("web: create tunnel: %v", err)
		errorTrigger(w, err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	renderTemplate(w, "tunnel.html", s.buildTunnelFragData())
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		errorTrigger(w, "tunnel manager not available")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := s.tunnel.Delete(r.Context()); err != nil {
		log.Printf("web: delete tunnel: %v", err)
		errorTrigger(w, err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	renderTemplate(w, "tunnel.html", s.buildTunnelFragData())
}
