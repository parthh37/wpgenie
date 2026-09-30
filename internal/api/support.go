package api

import "net/http"

// supportRoutes registers this feature's routes (mux: its public, unsigned
// endpoints). Empty until the feature lands here.
func (s *Server) supportRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {}
