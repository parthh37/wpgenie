package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/site"
)

// Uploads offload to S3-compatible storage. The secret key is write-only:
// no response includes it.

func (s *Server) offloadStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Sites.OffloadStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// setOffload checks the settings end to end (a test object written,
// fetched through the public URL and deleted): a few seconds.
func (s *Server) setOffload(w http.ResponseWriter, r *http.Request) error {
	var in site.OffloadInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetOffload(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) syncOffload(w http.ResponseWriter, r *http.Request) error {
	id, err := s.Sites.StartOffloadSync(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, map[string]int64{"job_id": id})
}

func (s *Server) downloadOffload(w http.ResponseWriter, r *http.Request) error {
	id, err := s.Sites.StartOffloadDownload(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, map[string]int64{"job_id": id})
}
