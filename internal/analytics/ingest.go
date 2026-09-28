package analytics

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

const stateName = "caddy_access_log"

// Ingester tails the access log and periodically commits rollups.
type Ingester struct {
	Path     string
	Store    *store.Store
	Secret   []byte
	Interval time.Duration
	// MaxBytesPerTick bounds memory and transaction size when catching up on
	// a large backlog (e.g. after downtime).
	MaxBytesPerTick int64
	Logger          *slog.Logger
}

func (in *Ingester) defaults() {
	if in.Interval == 0 {
		in.Interval = 10 * time.Second
	}
	if in.MaxBytesPerTick == 0 {
		in.MaxBytesPerTick = 64 << 20
	}
	if in.Logger == nil {
		in.Logger = slog.Default()
	}
}

func (in *Ingester) Run(ctx context.Context) error {
	in.defaults()
	st, err := in.Store.IngestState(ctx, stateName)
	if err != nil {
		return err
	}
	agg := newAggregator(in.Secret)
	t := time.NewTicker(in.Interval)
	defer t.Stop()
	for {
		if next, err := in.tick(ctx, st, agg); err != nil {
			in.Logger.Warn("analytics ingest", "err", err)
		} else {
			st = next
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (in *Ingester) tick(ctx context.Context, st store.IngestState, agg *aggregator) (store.IngestState, error) {
	in.defaults()
	domains, err := in.Store.DomainIndex(ctx)
	if err != nil {
		return st, err
	}
	next, err := readNew(in.Path, st, in.MaxBytesPerTick, func(line []byte) {
		e, ok := parseLine(line)
		if !ok {
			return
		}
		if site, ok := domains[normalizeHost(e.Request.Host)]; ok {
			agg.add(site, e)
		}
	})
	if err != nil {
		agg.reset()
		return st, err
	}
	if next == st {
		return st, nil
	}
	if err := in.Store.ApplyTraffic(ctx, agg.batch(next)); err != nil {
		return st, err
	}
	return next, nil
}

// readNew calls fn for every complete line appended since st. It detects
// rotation (inode change) and truncation (file smaller than offset) and
// restarts from the beginning of the new file in both cases. A trailing
// partial line is left for the next call.
func readNew(path string, st store.IngestState, maxBytes int64, fn func([]byte)) (store.IngestState, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil // Caddy hasn't logged anything yet
	}
	if err != nil {
		return st, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return st, err
	}
	inode := fileInode(fi)
	if inode != st.Inode || fi.Size() < st.Offset {
		st.Inode, st.Offset = inode, 0
	}
	if fi.Size() == st.Offset {
		return st, nil
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return st, err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, maxBytes), 256<<10)
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Absurdly long line: skip it rather than stall forever.
			st.Offset += int64(len(line))
			for err == bufio.ErrBufferFull {
				line, err = r.ReadSlice('\n')
				st.Offset += int64(len(line))
			}
			continue
		}
		if err != nil { // io.EOF: any partial line is left for next time
			return st, nil
		}
		st.Offset += int64(len(line))
		fn(line)
	}
}

func fileInode(fi os.FileInfo) uint64 {
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 0
}
