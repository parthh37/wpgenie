package api

import "net/http"

// invoicingRoutes registers this feature's routes (mux: its public, unsigned
// endpoints). Empty until the feature lands here.
func (s *Server) invoicingRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {}
