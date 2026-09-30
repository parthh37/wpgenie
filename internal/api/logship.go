package api

import "net/http"

// logshipRoutes registers this feature's routes (mux: its public, unsigned
// endpoints). Empty until the feature lands here.
func (s *Server) logshipRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {}
