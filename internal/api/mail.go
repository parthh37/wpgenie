package api

import (
	"net/http"

	"github.com/parthh37/wpgenie/internal/mail"
	"github.com/parthh37/wpgenie/internal/site"
)

func (s *Server) mailStatus(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, s.Mail.Status())
}

// setMail enables mail with a hostname, or disables it.
func (s *Server) setMail(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Enabled  bool   `json:"enabled"`
		Hostname string `json:"hostname"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sites, err := s.Store.ListSites(r.Context())
	if err != nil {
		return err
	}
	n := 0
	for _, st := range sites {
		if st.SMTP {
			n++
		}
	}
	if !in.Enabled {
		st, err := s.Mail.Disable(r.Context(), n)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, st)
	}
	host, err := site.NormalizeDomain(in.Hostname)
	if err != nil {
		return err
	}
	// One name can't be both a WordPress site and webmail in Caddy.
	if taken, err := s.Store.DomainExists(r.Context(), host); err != nil {
		return err
	} else if taken {
		return writeJSON(w, http.StatusConflict, map[string]string{"error": host + " is a site's domain; use a dedicated name such as mail." + host})
	}
	st, err := s.Mail.Enable(r.Context(), host, n)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) setRelay(w http.ResponseWriter, r *http.Request) error {
	var in mail.Relay
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Mail.SetRelay(r.Context(), &in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) deleteRelay(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Mail.SetRelay(r.Context(), nil)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) mailDomains(w http.ResponseWriter, r *http.Request) error {
	ds, err := s.Mail.Domains(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ds)
}

func (s *Server) addMailDomain(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Domain string `json:"domain"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	d, err := s.Mail.AddDomain(r.Context(), in.Domain)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, d)
}

// mailDomain returns the DNS records to publish; ?check=1 compares them
// with live DNS.
func (s *Server) mailDomain(w http.ResponseWriter, r *http.Request) error {
	d, err := s.Mail.Domain(r.Context(), r.PathValue("domain"), r.URL.Query().Get("check") == "1")
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteMailDomain(w http.ResponseWriter, r *http.Request) error {
	if err := s.Mail.DeleteDomain(r.Context(), r.PathValue("domain")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) mailboxes(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Mail.Mailboxes(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, m)
}

// createMailbox returns the password exactly once; the panel never stores it.
func (s *Server) createMailbox(w http.ResponseWriter, r *http.Request) error {
	var in mail.MailboxInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	m, pw, err := s.Mail.CreateMailbox(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, map[string]any{"mailbox": m, "password": pw})
}

func (s *Server) setMailboxPassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"` // empty: generate one
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	pw, err := s.Mail.SetPassword(r.Context(), r.PathValue("address"), in.Password)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) setMailboxQuota(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		QuotaMB int `json:"quota_mb"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.Mail.SetQuota(r.Context(), r.PathValue("address"), in.QuotaMB); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) deleteMailbox(w http.ResponseWriter, r *http.Request) error {
	if err := s.Mail.DeleteMailbox(r.Context(), r.PathValue("address")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) mailAliases(w http.ResponseWriter, r *http.Request) error {
	a, err := s.Mail.Aliases(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, a)
}

func (s *Server) addMailAlias(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Alias  string `json:"alias"`
		Target string `json:"target"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	a, err := s.Mail.AddAlias(r.Context(), in.Alias, in.Target)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, a)
}

func (s *Server) deleteMailAlias(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	if err := s.Mail.DeleteAlias(r.Context(), q.Get("alias"), q.Get("target")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) setSiteSMTP(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if s.Cluster != nil {
		if node, remote, err := s.Cluster.SiteNode(r.Context(), r.PathValue("id")); err != nil {
			return err
		} else if remote {
			return s.setRemoteSMTP(w, r, node, in.Enabled)
		}
	}
	st, err := s.Sites.SetSMTP(r.Context(), r.PathValue("id"), in.Enabled)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}
