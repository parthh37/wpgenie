package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/parthh37/wpgenie/internal/site"
)

// setImages chooses the formats uploads are served in; converting runs as
// a job (job_id 0: nothing to do).
func (s *Server) setImages(w http.ResponseWriter, r *http.Request) error {
	var in site.ImagesInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, jobID, err := s.Sites.SetImages(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"site": st, "job_id": jobID})
}

func (s *Server) convertImages(w http.ResponseWriter, r *http.Request) error {
	id, err := s.Sites.StartImageConversion(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, map[string]int64{"job_id": id})
}

// insights: response times, cache hits, slow URLs and PHP errors over the
// last ?hours= (default 24, up to 90 days).
func (s *Server) insights(w http.ResponseWriter, r *http.Request) error {
	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 1 || n > 24*90 {
			return errBadRequest
		}
		hours = n
	}
	out, err := s.Sites.Insights(r.Context(), r.PathValue("id"), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// clearPHPErrors forgets a site's PHP errors, e.g. after fixing them.
func (s *Server) clearPHPErrors(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	if err := s.Store.ClearPHPErrors(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
