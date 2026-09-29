package store

import (
	"context"
	"errors"
	"time"
)

var ErrExists = errors.New("already exists")

type MailDomain struct {
	Domain    string    `json:"domain"`
	CreatedAt time.Time `json:"created_at"`
}

type Mailbox struct {
	Address   string    `json:"address"`
	Domain    string    `json:"domain"`
	QuotaMB   int       `json:"quota_mb"` // 0 = unlimited
	SiteID    string    `json:"site_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type MailAlias struct {
	Alias  string `json:"alias"`
	Target string `json:"target"`
	Domain string `json:"domain"`
}

func (s *Store) AddMailDomain(ctx context.Context, domain string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mail_domains (domain, created_at) VALUES (?, ?)`, domain, time.Now().Unix())
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) MailDomains(ctx context.Context) ([]MailDomain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, created_at FROM mail_domains ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailDomain{}
	for rows.Next() {
		var d MailDomain
		var at int64
		if err := rows.Scan(&d.Domain, &at); err != nil {
			return nil, err
		}
		d.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) MailDomainExists(ctx context.Context, domain string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_domains WHERE domain = ?`, domain).Scan(&n)
	return n > 0, err
}

// DeleteMailDomain removes a domain that has no mailboxes or aliases left.
func (s *Store) DeleteMailDomain(ctx context.Context, domain string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM mailboxes WHERE domain = ?) +
		(SELECT COUNT(*) FROM mail_aliases WHERE domain = ?)`, domain, domain).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("the domain still has mailboxes or aliases")
	}
	return s.exec1(ctx, `DELETE FROM mail_domains WHERE domain = ?`, domain)
}

func (s *Store) AddMailbox(ctx context.Context, m Mailbox) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mailboxes (address, domain, quota_mb, site_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		m.Address, m.Domain, m.QuotaMB, m.SiteID, time.Now().Unix())
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) Mailboxes(ctx context.Context, domain string) ([]Mailbox, error) {
	q := `SELECT address, domain, quota_mb, site_id, created_at FROM mailboxes`
	var args []any
	if domain != "" {
		q += ` WHERE domain = ?`
		args = append(args, domain)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY address`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Mailbox{}
	for rows.Next() {
		var m Mailbox
		var at int64
		if err := rows.Scan(&m.Address, &m.Domain, &m.QuotaMB, &m.SiteID, &at); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMailbox(ctx context.Context, address string) (*Mailbox, error) {
	boxes, err := s.Mailboxes(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, m := range boxes {
		if m.Address == address {
			return &m, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) SetMailboxQuota(ctx context.Context, address string, quotaMB int) error {
	return s.exec1(ctx, `UPDATE mailboxes SET quota_mb = ? WHERE address = ?`, quotaMB, address)
}

func (s *Store) DeleteMailbox(ctx context.Context, address string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM mailboxes WHERE address = ?`, address)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// The mail server drops aliases pointing at a deleted account too.
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_aliases WHERE target = ?`, address); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddMailAlias(ctx context.Context, a MailAlias) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mail_aliases (alias, target, domain) VALUES (?, ?, ?)`, a.Alias, a.Target, a.Domain)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) MailAliases(ctx context.Context) ([]MailAlias, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alias, target, domain FROM mail_aliases ORDER BY alias, target`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailAlias{}
	for rows.Next() {
		var a MailAlias
		if err := rows.Scan(&a.Alias, &a.Target, &a.Domain); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMailAlias(ctx context.Context, alias, target string) error {
	return s.exec1(ctx, `DELETE FROM mail_aliases WHERE alias = ? AND target = ?`, alias, target)
}
