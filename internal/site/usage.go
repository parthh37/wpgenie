package site

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// DBSizer reports how much space a database takes (optional on
// DBProvisioner: dbprov.MariaDB implements it).
type DBSizer interface {
	DatabaseSize(ctx context.Context, db string) (int64, error)
}

// MeasureDisk measures a site's disk use: every regular file under its
// directory (WordPress, uploads, logs, wp-config.php) and its database's
// data and indexes. The walk runs through os.Root and never follows a
// symlink (a site can plant them anywhere in its docroot): a link to
// another site, or to /, counts as the few bytes of the link itself.
// Sizes are apparent sizes (what the files hold), not allocated blocks.
func (s *Service) MeasureDisk(ctx context.Context, id string) (store.SiteUsage, error) {
	u := store.SiteUsage{SiteID: id}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return u, err
	}
	if u.FilesBytes, err = dirSize(ctx, s.Cfg.SiteDir(id)); err != nil {
		return u, err
	}
	if sizer, ok := s.DB.(DBSizer); ok {
		if u.DBBytes, err = sizer.DatabaseSize(ctx, st.DBName); err != nil {
			return u, err
		}
	}
	u.MeasuredAt = time.Now().UTC()
	return u, nil
}

// dirSize sums the sizes of the regular files under dir without leaving it.
func dirSize(ctx context.Context, dir string) (int64, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // never provisioned, or its files are elsewhere
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	var total int64
	var n int
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if n++; n%1000 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == "." {
				return err
			}
			return nil // vanished or unreadable meanwhile: the site is live
		}
		// WalkDir only descends into real directories: a symlink's type is
		// ModeSymlink, never a directory, whatever it points at.
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info() // lstat: the entry itself
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// InMaintenanceWindow reports whether now is in the nightly maintenance
// window (maintenance_hour), when heavy periodic work runs.
func (s *Service) InMaintenanceWindow(now time.Time) bool { return s.inMaintenanceWindow(now) }
