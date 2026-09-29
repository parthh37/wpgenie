// Package updater updates WPGenie itself ("over the air"): it finds the
// latest GitHub release, verifies it against the release signing key built
// into this binary, stages it, and hands over to an applier that swaps it
// in and rolls back if the new version doesn't come up healthy.
//
// Trust chain: release.pub (compiled in) -> Ed25519 signature over
// checksums.txt -> SHA-256 of the release tarball. Neither GitHub nor the
// network is trusted: a tampered asset fails verification and nothing is
// installed.
package updater

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// releasePub is the PEM public key releases are signed with. Generate the
// pair with `make release-key`; an empty file disables self-update.
//
//go:embed release.pub
var releasePub []byte

var (
	ErrNoSigningKey = errors.New("this build has no release signing key (internal/updater/release.pub); self-update is disabled")
	ErrBadSignature = errors.New("release signature does not verify: refusing to install")
	ErrUpToDate     = errors.New("already running the latest release")
	ErrNotService   = errors.New("self-update needs WPGenie running as the systemd service installed by install.sh")
	ErrBusy         = errors.New("an update is already in progress")
)

// PublicKey parses the compiled-in signing key.
func PublicKey() (ed25519.PublicKey, error) { return parsePublicKey(releasePub) }

func parsePublicKey(b []byte) (ed25519.PublicKey, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, ErrNoSigningKey
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("release.pub is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("release.pub is not an Ed25519 key")
	}
	return pub, nil
}

// Release is a published WPGenie version.
type Release struct {
	Version     string    `json:"version"` // tag, e.g. v0.3.0
	Notes       string    `json:"notes"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	assets      map[string]string
}

// Info is what the panel shows about updates.
type Info struct {
	Current   string    `json:"current"`
	Latest    *Release  `json:"latest"`
	Available bool      `json:"available"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	CheckErr  string    `json:"check_error,omitempty"`
	Signed    bool      `json:"signing_key"` // false: this build can't self-update
	Status    *Status   `json:"status"`
}

type Updater struct {
	Current  string // running version (main.version)
	Repo     string // owner/name on GitHub
	StateDir string // e.g. /var/lib/wpgenie/updates
	Client   *http.Client
	APIBase  string // default https://api.github.com
	// Launch starts the applier for a staged release; default runs it as a
	// transient systemd unit (see launch.go).
	Launch func(ctx context.Context, staged string) error

	mu        sync.Mutex
	latest    *Release
	checkedAt time.Time
	checkErr  error
	applying  bool
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// Check asks GitHub for the latest release and remembers the answer.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	r, err := u.fetchLatest(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checkedAt, u.checkErr = time.Now().UTC(), err
	if err == nil {
		u.latest = r
	}
	return r, err
}

func (u *Updater) fetchLatest(ctx context.Context) (*Release, error) {
	base := u.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+u.Repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "WPGenie/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("no published release yet")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub: HTTP %d", resp.StatusCode)
	}
	var gh struct {
		TagName     string    `json:"tag_name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gh); err != nil {
		return nil, err
	}
	if !tagRe(gh.TagName) {
		return nil, fmt.Errorf("unexpected release tag %q", gh.TagName)
	}
	// Shown as a link in the panel; unsigned data, so only a GitHub page.
	if !strings.HasPrefix(gh.HTMLURL, "https://github.com/") {
		gh.HTMLURL = "https://github.com/" + u.Repo + "/releases"
	}
	r := &Release{Version: gh.TagName, Notes: gh.Body, URL: gh.HTMLURL, PublishedAt: gh.PublishedAt, assets: map[string]string{}}
	for _, a := range gh.Assets {
		r.assets[a.Name] = a.URL
	}
	return r, nil
}

// Info reports the running version, the latest known release and the
// status of the last update.
func (u *Updater) Info() Info {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, keyErr := PublicKey()
	in := Info{Current: u.Current, Latest: u.latest, CheckedAt: u.checkedAt, Signed: keyErr == nil}
	if u.checkErr != nil {
		in.CheckErr = u.checkErr.Error()
	}
	in.Available = u.latest != nil && Newer(u.latest.Version, u.Current)
	if st, err := ReadStatus(u.StateDir); err == nil {
		in.Status = st
	}
	return in
}

// Run re-checks for releases every interval until ctx ends.
func (u *Updater) Run(ctx context.Context, interval time.Duration) {
	for {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		_, _ = u.Check(c) // the error is kept for Info
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Update downloads, verifies and stages the latest release, then launches
// the applier, which restarts WPGenie. It returns once the applier is
// running; follow progress with Info().Status.
func (u *Updater) Update(ctx context.Context) (string, error) {
	pub, err := PublicKey()
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	if u.applying {
		u.mu.Unlock()
		return "", ErrBusy
	}
	u.applying = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.applying = false
		u.mu.Unlock()
	}()
	if st, err := ReadStatus(u.StateDir); err == nil && st.Active() {
		return "", ErrBusy
	}

	rel, err := u.Check(ctx)
	if err != nil {
		return "", err
	}
	if !Newer(rel.Version, u.Current) {
		return "", ErrUpToDate
	}
	// Releases staged by earlier updates are finished with (no update is
	// active); only status.json carries over.
	if entries, err := os.ReadDir(u.StateDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				os.RemoveAll(filepath.Join(u.StateDir, e.Name()))
			}
		}
	}
	staged, err := u.stage(ctx, rel, pub)
	if err != nil {
		WriteStatus(u.StateDir, &Status{Phase: PhaseFailed, From: u.Current, To: rel.Version,
			Message: "download failed, nothing was changed: " + err.Error()})
		return "", err
	}
	if err := WriteStatus(u.StateDir, &Status{Phase: PhaseStaged, From: u.Current, To: rel.Version,
		Message: "verified and staged; installing"}); err != nil {
		return "", err
	}
	launch := u.Launch
	if launch == nil {
		launch = LaunchSystemd
	}
	if err := launch(ctx, staged); err != nil {
		WriteStatus(u.StateDir, &Status{Phase: PhaseFailed, From: u.Current, To: rel.Version,
			Message: "could not start the installer: " + err.Error()})
		return "", err
	}
	return rel.Version, nil
}

func archiveName(version string) string {
	return "wpgenie_" + version + "_linux_" + goruntime.GOARCH + ".tar.gz"
}

// stage downloads the release into StateDir/<version>, verifying the
// signature and checksum before extracting anything.
func (u *Updater) stage(ctx context.Context, rel *Release, pub ed25519.PublicKey) (string, error) {
	sums, err := u.download(ctx, rel, "checksums.txt", 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := u.download(ctx, rel, "checksums.txt.sig", 1<<10)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(pub, sums, sig) {
		return "", ErrBadSignature
	}
	name := archiveName(rel.Version)
	want, err := checksumFor(sums, name)
	if err != nil {
		return "", err
	}
	archive, err := u.download(ctx, rel, name, 512<<20)
	if err != nil {
		return "", err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("%s: checksum mismatch: refusing to install", name)
	}
	dir := filepath.Join(u.StateDir, rel.Version)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := extract(bytes.NewReader(archive), dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("extracting %s: %w", name, err)
	}
	for _, need := range []string{"wpgenie", "deploy/docker-compose.yml", "deploy/wpgenie.service", "images/php/Dockerfile"} {
		if _, err := os.Stat(filepath.Join(dir, need)); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("release is missing %s", need)
		}
	}
	return dir, os.WriteFile(filepath.Join(dir, "VERSION"), []byte(rel.Version), 0o600)
}

func (u *Updater) download(ctx context.Context, rel *Release, asset string, limit int64) ([]byte, error) {
	url, ok := rel.assets[asset]
	if !ok {
		return nil, fmt.Errorf("release %s has no %s (unsigned or incomplete release)", rel.Version, asset)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "WPGenie/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", asset, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", asset, limit)
	}
	return b, nil
}

// checksumFor finds name in sha256sum output.
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

// extract unpacks a release archive. Only regular files and directories
// under the expected top-level names are accepted: no links, no absolute
// paths, no "..", no device files, bounded total size.
func extract(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		top, _, _ := strings.Cut(name, "/")
		switch top {
		case "wpgenie", "LICENSE", "README.md", "deploy", "images":
		default:
			return fmt.Errorf("unexpected entry %q", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; total > 1<<30 {
				return errors.New("archive too large")
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry type for %q", h.Name)
		}
	}
}

// preRe: pre-release identifiers per semver. Tags end up in paths
// (StateDir/<tag>), so nothing like "/" or ".." may pass.
var preRe = regexp.MustCompile(`^[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*$`)

func tagRe(tag string) bool {
	_, ok := parseSemver(tag)
	return ok
}

// parseSemver parses vMAJOR.MINOR.PATCH[-pre]. Pre-release parts are kept
// only to rank below the release.
func parseSemver(v string) ([4]int, bool) {
	var out [4]int
	v, ok := strings.CutPrefix(v, "v")
	if !ok {
		return out, false
	}
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	out[3] = 1 // releases rank above their pre-releases
	if hasPre {
		// `git describe` output (v0.1.0-12-gabc1234[-dirty]) is a local
		// build, not a release.
		if pre == "" || strings.Contains(pre, "-g") || strings.HasSuffix(pre, "dirty") || !preRe.MatchString(pre) {
			return out, false
		}
		out[3] = 0
	}
	return out, true
}

// Newer reports whether release is newer than current. Development builds
// ("dev", git describe output) never self-update: they aren't releases.
func Newer(release, current string) bool {
	r, ok1 := parseSemver(release)
	c, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range r {
		if r[i] != c[i] {
			return r[i] > c[i]
		}
	}
	return false
}
