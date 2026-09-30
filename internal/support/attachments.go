package support

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/store"
)

// Attachments are uploaded into a staging folder first (the ticket or the
// message they belong to doesn't exist yet), checked, and moved next to
// their ticket once it's recorded: Dir/<ticket>/<random name>. The name
// the uploader gave is only ever displayed (and sent back in
// Content-Disposition), never used as a path.

// Staged is an uploaded file, checked, waiting for its message.
type Staged struct {
	Name        string // the uploader's, cleaned up
	ContentType string // sniffed from the content
	Size        int64
	file        string // random name
	path        string // in the staging folder
}

// sniffed are the types a file with these extensions must look like (the
// content decides, not the name): a PNG that's really an HTML page is
// refused. Other allowed extensions only need to not look like a web page.
var sniffed = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp",
	"pdf": "application/pdf", "zip": "application/zip", "txt": "text/plain", "log": "text/plain", "csv": "text/plain",
}

var randomName = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Service) stagingDir() string { return filepath.Join(s.Dir, ".staging") }

// cleanName is the display name of an upload: its last path element,
// without control characters, at most 120 characters.
func cleanName(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	name = strings.TrimSpace(name)
	for utf8.RuneCountInString(name) > 120 {
		// Keep the extension: shorten the stem.
		ext := filepath.Ext(name)
		if len(ext) > 12 {
			ext = ""
		}
		r := []rune(strings.TrimSuffix(name, ext))
		name = string(r[:120-utf8.RuneCountInString(ext)]) + ext
	}
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

func newFileName() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Stage reads an upload into the staging folder, within the settings'
// limits (size, allowed types), and checks that its content is what its
// name says. The caller hands the result to Open or Reply, or Discards it.
func (s *Service) Stage(st *Settings, name string, r io.Reader) (*Staged, error) {
	name = cleanName(name)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" || !slices.Contains(st.Extensions, ext) || activeTypes[ext] {
		return nil, fmt.Errorf("%w: %s: this type of file can't be attached (allowed: %s)", ErrInvalid, name,
			strings.Join(st.Extensions, ", "))
	}
	if err := os.MkdirAll(s.stagingDir(), 0o700); err != nil {
		return nil, err
	}
	file, err := newFileName()
	if err != nil {
		return nil, err
	}
	f := &Staged{Name: name, file: file, path: filepath.Join(s.stagingDir(), file)}
	out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	limit := int64(st.MaxFileMB) << 20
	head := make([]byte, 512)
	n, rerr := io.ReadFull(r, head)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		out.Close()
		os.Remove(f.path)
		return nil, rerr
	}
	head = head[:n]
	written, err := out.Write(head)
	if err == nil {
		var rest int64
		rest, err = io.Copy(out, io.LimitReader(r, limit+1-int64(written)))
		f.Size = int64(written) + rest
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	fail := func(e error) (*Staged, error) {
		os.Remove(f.path)
		return nil, e
	}
	if err != nil {
		return fail(err)
	}
	if f.Size > limit {
		return fail(fmt.Errorf("%w: %s is larger than %d MB", ErrTooLarge, name, st.MaxFileMB))
	}
	if f.Size == 0 {
		return fail(fmt.Errorf("%w: %s is empty", ErrInvalid, name))
	}
	f.ContentType, _, _ = strings.Cut(http.DetectContentType(head), ";")
	if want, ok := sniffed[ext]; ok && f.ContentType != want {
		return fail(fmt.Errorf("%w: %s isn't a .%s file", ErrInvalid, name, ext))
	}
	if f.ContentType == "text/html" || f.ContentType == "text/xml" {
		return fail(fmt.Errorf("%w: %s looks like a web page; it can't be attached", ErrInvalid, name))
	}
	return f, nil
}

// Discard removes staged files that weren't kept (a no-op for kept ones).
func (s *Service) Discard(files []*Staged) {
	for _, f := range files {
		if f != nil && f.path != "" {
			os.Remove(f.path)
		}
	}
}

func attachmentRows(files []*Staged, at time.Time) []*store.TicketAttachment {
	out := make([]*store.TicketAttachment, 0, len(files))
	for _, f := range files {
		out = append(out, &store.TicketAttachment{Name: f.Name, File: f.file, ContentType: f.ContentType, Size: f.Size, CreatedAt: at})
	}
	return out
}

func (s *Service) ticketDir(id int64) string { return filepath.Join(s.Dir, strconv.FormatInt(id, 10)) }

// keep moves a recorded message's staged files next to their ticket. A
// file that can't be moved loses its record: the message stays, and the
// returned text says what's missing ("" when everything was kept).
func (s *Service) keep(ctx context.Context, ticketID int64, files []*Staged, rows []*store.TicketAttachment) string {
	if len(files) == 0 {
		return ""
	}
	dir := s.ticketDir(ticketID)
	err := os.MkdirAll(dir, 0o700)
	var lost []int64
	var names []string
	for i, f := range files {
		if err == nil {
			err = os.Rename(f.path, filepath.Join(dir, f.file))
		}
		if err != nil {
			s.Log.Error("support: storing an attachment", "ticket", ticketID, "name", f.Name, "err", err)
			lost, names = append(lost, rows[i].ID), append(names, f.Name)
			err = nil
			continue
		}
		f.path = "" // kept: Discard leaves it
	}
	if len(lost) == 0 {
		return ""
	}
	if err := s.Store.DeleteTicketAttachments(context.WithoutCancel(ctx), lost...); err != nil {
		s.Log.Error("support: forgetting lost attachments", "ticket", ticketID, "err", err)
	}
	return "The message was saved, but these attachments couldn't be stored: " + strings.Join(names, ", ")
}

// Attachment opens an attachment of a ticket for a party to it; an
// attachment of a message the party doesn't see (an internal note for the
// customer, a staff-only note for the reseller) is not there.
func (s *Service) Attachment(ctx context.Context, a Actor, ticketID, id int64) (*store.TicketAttachment, *os.File, error) {
	_, p, err := s.ticket(ctx, a, ticketID)
	if err != nil {
		return nil, nil, err
	}
	att, err := s.Store.GetTicketAttachment(ctx, ticketID, id)
	if err != nil {
		return nil, nil, err
	}
	if p != PartyStaff {
		m, err := s.Store.GetTicketMessage(ctx, ticketID, att.MessageID)
		if err != nil {
			return nil, nil, err
		}
		if !visible(m, p) {
			return nil, nil, store.ErrNotFound
		}
	}
	if !randomName.MatchString(att.File) {
		return nil, nil, store.ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.ticketDir(ticketID), att.File))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, store.ErrNotFound
	}
	return att, f, err
}

// CleanStaging removes staged files an interrupted request left behind.
func (s *Service) CleanStaging(olderThan time.Duration) {
	entries, err := os.ReadDir(s.stagingDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && s.now().Sub(info.ModTime()) > olderThan {
			os.Remove(filepath.Join(s.stagingDir(), e.Name()))
		}
	}
}

// RemoveTicketFiles deletes tickets' attachment folders (their tickets
// deleted with their account). Best effort: failures are logged.
func (s *Service) RemoveTicketFiles(ids []int64) {
	for _, id := range ids {
		if err := os.RemoveAll(s.ticketDir(id)); err != nil {
			s.Log.Warn("support: removing a deleted ticket's attachments", "ticket", id, "err", err)
		}
	}
}

// RemoveOrphanFiles deletes the attachment folders of tickets that no
// longer exist (a deletion whose clean-up didn't finish), and reports
// how many.
func (s *Service) RemoveOrphanFiles(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	var ids []int64
	for _, e := range entries {
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && id > 0 && e.IsDir() && strconv.FormatInt(id, 10) == e.Name() {
			ids = append(ids, id)
		}
	}
	exist, err := s.Store.ExistingTicketIDs(ctx, ids)
	if err != nil {
		return 0, err
	}
	var gone []int64
	for _, id := range ids {
		if !exist[id] {
			gone = append(gone, id)
		}
	}
	s.RemoveTicketFiles(gone)
	return len(gone), nil
}
