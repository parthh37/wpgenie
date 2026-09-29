package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// Image optimisation: JPEG/PNG uploads get AVIF and/or WebP copies next to
// them (<file>.avif, <file>.webp), which Caddy serves in place of the
// original to browsers that accept them. URLs, HTML and the page cache
// don't change. The conversion is PHP + GD inside the site's container, as
// the site user, under the cron jail: images.php for new uploads (by cron),
// convert-images.php for what is already there (a job when turned on or
// asked for, and nightly for files that arrived by SFTP or a restore).

const (
	imagesWrapperPath = "wp-content/mu-plugins/wpgenie-images.php"
	convertScript     = "/usr/local/share/wpgenie/convert-images.php"
	// convertTimeout bounds one run asked for from the panel; the nightly
	// catch-up gets nightlyConvertTimeout (it holds a heavy job slot, which
	// the night's backups share). What's left converts the next night.
	convertTimeout        = 2 * 3600
	nightlyConvertTimeout = 20 * 60
	noScriptExit          = 99
)

// ImageFormats WPGenie can convert uploads to, best first.
var ImageFormats = []string{"avif", "webp"}

const imagesWrapper = `<?php
/**
 * Plugin Name: WPGenie Images
 * Description: Serves uploads as AVIF/WebP to browsers that accept them. Managed by WPGenie: change it in the WPGenie panel; this file is rewritten on changes.
 */
define( 'WPGENIE_IMAGE_FORMATS', '%s' );
if ( is_file( '/usr/local/share/wpgenie/images.php' ) ) {
	require_once '/usr/local/share/wpgenie/images.php';
}
`

type ImagesInput struct {
	Formats []string `json:"formats"` // "avif", "webp"; empty: off
}

// ImageSummary is what a conversion run did (convert-images.php).
type ImageSummary struct {
	Images      int      `json:"images"`
	Converted   int      `json:"converted"`
	Current     int      `json:"current"`
	Larger      int      `json:"larger"`
	Skipped     int      `json:"skipped"`
	Removed     int      `json:"removed"`
	BytesBefore int64    `json:"bytes_before"`
	BytesAfter  int64    `json:"bytes_after"`
	Formats     []string `json:"formats"`
	Partial     bool     `json:"partial"`
}

// normalizeFormats validates formats and puts them in preference order.
func normalizeFormats(in []string) ([]string, error) {
	for _, f := range in {
		if !slices.Contains(ImageFormats, f) {
			return nil, fmt.Errorf("%w: image format %q (want avif and/or webp)", ErrInvalidInput, f)
		}
	}
	out := []string{}
	for _, f := range ImageFormats {
		if slices.Contains(in, f) {
			out = append(out, f)
		}
	}
	return out, nil
}

// SetImages chooses the formats a site's uploads are converted to and
// served in, then converts (or removes the copies no longer wanted) as a
// job, whose ID it returns (0 when nothing needed doing).
func (s *Service) SetImages(ctx context.Context, id string, in ImagesInput) (*store.Site, int64, error) {
	formats, err := normalizeFormats(in.Formats)
	if err != nil {
		return nil, 0, err
	}
	retire, prev, err := s.setImagesLocked(ctx, id, formats)
	if err != nil {
		return nil, 0, err
	}
	retire()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	var jobID int64
	if !slices.Equal(prev, formats) {
		if jobID, err = s.StartImageConversion(ctx, id); err != nil {
			return st, 0, err
		}
		if len(formats) == 0 {
			s.event(id, "images", "Image optimisation off: removing converted copies")
		} else {
			s.event(id, "images", "Image optimisation on: uploads are served as "+strings.ToUpper(strings.Join(formats, ", ")))
		}
	}
	return st, jobID, nil
}

func (s *Service) setImagesLocked(ctx context.Context, id string, formats []string) (func(), []string, error) {
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, nil, fmt.Errorf("%w: an update, scan or job is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if st.Status != store.StatusActive {
		return nil, nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	prev := st.ImageFormats
	if err := s.writeImagesWrapper(id, formats); err != nil {
		return nil, nil, err
	}
	if err := s.Store.SetImageFormats(ctx, id, formats); err != nil {
		return nil, nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, nil, err
	}
	// Replicas from an image without the converter would ignore the new
	// wrapper: roll any that are out of date.
	st.ImageFormats = formats
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		return nil, nil, fmt.Errorf("image settings saved, but refreshing the site's PHP containers failed: %w", err)
	}
	return retire, prev, nil
}

func (s *Service) writeImagesWrapper(id string, formats []string) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	// Formats are validated: only letters and commas reach the PHP string.
	return ensureManaged(root, imagesWrapperPath, fmt.Sprintf(imagesWrapper, strings.Join(formats, ",")), len(formats) > 0)
}

// StartImageConversion converts the site's uploads to its formats (and
// removes copies that no longer belong) as a job.
func (s *Service) StartImageConversion(ctx context.Context, id string) (int64, error) {
	return s.startImageConversion(ctx, id, convertTimeout)
}

func (s *Service) startImageConversion(ctx context.Context, id string, timeout int) (int64, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return 0, err
	}
	if st.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	// Heavy: it keeps a CPU busy for as long as it takes.
	return s.Jobs.Submit(ctx, s.siteJob(id, "images", true), func(ctx context.Context, t *jobs.Task) error {
		st, err := s.Store.GetSite(ctx, id)
		if err != nil {
			return err
		}
		t.Progress(1, "Looking for images")
		sum, err := s.convertImages(ctx, st, t.Progress, timeout)
		if err != nil {
			return err
		}
		t.SetResult(sum)
		s.imagesEvent(id, sum)
		return nil
	})
}

func (s *Service) imagesEvent(id string, sum *ImageSummary) {
	if sum.Converted == 0 && sum.Removed == 0 && !sum.Partial {
		return
	}
	msg := fmt.Sprintf("Images: %d converted, %d up to date, %d kept as they were (no smaller), %d skipped, %d copies removed",
		sum.Converted, sum.Current, sum.Larger, sum.Skipped, sum.Removed)
	if sum.BytesBefore > 0 {
		msg += fmt.Sprintf("; %s of uploads served as %s", fmtMB(sum.BytesBefore), fmtMB(sum.BytesAfter))
	}
	if sum.Partial {
		msg += " (stopped at its time limit: the rest is converted tonight)"
	}
	s.event(id, "images", msg)
}

func fmtMB(b int64) string { return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + " MB" }

// convertImages runs convert-images.php in the site's container: niced
// (visitors' requests share the container's CPU), jailed like cron, as the
// site user. Called with the site's maintenance lock held.
func (s *Service) convertImages(ctx context.Context, st *store.Site, report Progress, timeout int) (*ImageSummary, error) {
	var sum *ImageSummary
	lines := &lineWriter{fn: func(line []byte) {
		var m struct {
			Done    int           `json:"done"`
			Total   int           `json:"total"`
			Summary *ImageSummary `json:"summary"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		if m.Summary != nil {
			sum = m.Summary
		} else if m.Total > 0 {
			report(1+98*m.Done/m.Total, fmt.Sprintf("Converting images: %d of %d", m.Done, m.Total))
		}
	}}
	uploads := path.Join(s.Cfg.SiteRoot(st.ID), "wp-content", "uploads")
	err := s.Runtime.Exec(ctx, st.ID, nil, lines, "sh", "-c",
		`[ -f "$1" ] || exit `+strconv.Itoa(noScriptExit)+`; exec env PHP_INI_SCAN_DIR=:/usr/local/etc/php/jail.d nice -n 19 timeout "$2" php "$1" "$3" "$4"`,
		"sh", convertScript, strconv.Itoa(timeout), uploads, strings.Join(st.ImageFormats, ","))
	lines.flush()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == noScriptExit:
		return nil, fmt.Errorf("%w: the site's PHP containers predate image optimisation; roll them onto the current image (wpgenie site scale %s)", ErrConflict, st.ID)
	case errors.As(err, &ee) && ee.ExitCode() == 124 && sum == nil:
		return &ImageSummary{Formats: st.ImageFormats, Partial: true}, nil
	case err != nil:
		return nil, fmt.Errorf("converting images: %w", err)
	case sum == nil:
		return nil, errors.New("converting images: no summary from the converter")
	}
	return sum, nil
}

// lineWriter calls fn for every complete line written to it.
type lineWriter struct {
	buf []byte
	fn  func([]byte)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 1<<16 {
		w.buf = nil // not ours: the converter's lines are short
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.fn(w.buf)
		w.buf = nil
	}
}
