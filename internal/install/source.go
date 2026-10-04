package install

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultReleaseURL is where release assets are downloaded from:
// <base>/v<version>/<asset>.
const DefaultReleaseURL = "https://github.com/jerrykal/tower/releases/download"

// Source finds the build of one version for a platform.
type Source struct {
	Version    string
	Self       string // this binary, the build for its own platform
	Dir        string // the dist cache of Version
	ReleaseURL string // base of the release downloads
	HTTP       *http.Client

	mu sync.Mutex // one fill of the cache at a time
}

// NewSource is the source of version: self for this platform, the dist
// cache (TOWER_DIST_DIR), the release (TOWER_RELEASE_URL).
func NewSource(version, self string) *Source {
	u := os.Getenv("TOWER_RELEASE_URL")
	if u == "" {
		u = DefaultReleaseURL
	}
	return &Source{Version: version, Self: self, Dir: cacheDir(version), ReleaseURL: strings.TrimSuffix(u, "/"),
		HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// Archive is the release asset name of version for p.
func Archive(version string, p Platform) string {
	return "tower_" + version + "_" + p.OS + "_" + p.Arch + ".tar.gz"
}

// dev reports whether version is a development build, which has no
// release to download.
func dev(version string) bool { return strings.Contains(version, "-dev") }

// Binary is a local file holding the build for p: this binary for its own
// platform, else the binary of the cached archive, downloading and
// verifying the archive first when the cache lacks it.
func (s *Source) Binary(ctx context.Context, p Platform) (string, error) {
	if p == Local() {
		return s.Self, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := Archive(s.Version, p)
	arch := filepath.Join(s.Dir, name)
	// Extracted afresh every time: a rebuilt dev archive keeps its name.
	bin := filepath.Join(s.Dir, strings.TrimSuffix(name, ".tar.gz"), "tower")
	if _, err := os.Stat(arch); err != nil {
		if dev(s.Version) {
			return "", fmt.Errorf("no tower %s build for %s: run mise run dist", s.Version, p)
		}
		if err := s.download(ctx, name); err != nil {
			return "", fmt.Errorf("cannot download tower %s for %s: %w", s.Version, p, err)
		}
	} else if err := s.verifyCached(name); err != nil {
		return "", err
	}
	if err := extract(arch, bin); err != nil {
		return "", fmt.Errorf("tower %s for %s: %w", s.Version, p, err)
	}
	return bin, nil
}

// download fetches the archive and checksums.txt of the release, and
// keeps the archive only if its checksum matches.
func (s *Source) download(ctx context.Context, name string) error {
	base := s.ReleaseURL + "/v" + s.Version + "/"
	sums, err := s.get(ctx, base+"checksums.txt")
	if err != nil {
		return err
	}
	want, ok := checksum(sums, name)
	if !ok {
		return fmt.Errorf("checksums.txt has no %s", name)
	}
	data, err := s.get(ctx, base+name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s): refused", name, got[:12], want[:min(12, len(want))])
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	// Its own checksum file: the cache's checksums.txt may list archives
	// built here (mise run dist), whose bytes differ from the release's.
	if err := writeAtomic(filepath.Join(s.Dir, name+".sha256"), []byte(want+"  "+name+"\n"), 0o644); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.Dir, name), data, 0o644)
}

// verifyCached checks a cached archive against its checksum: its own
// <name>.sha256 (a download), else the cache's checksums.txt (mise run
// dist). An archive with neither is refused: nothing unverified is run on
// a host.
func (s *Source) verifyCached(name string) error {
	want, ok := "", false
	for _, f := range []string{name + ".sha256", "checksums.txt"} {
		sums, err := os.ReadFile(filepath.Join(s.Dir, f))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if want, ok = checksum(sums, name); ok {
			break
		}
	}
	if !ok {
		return fmt.Errorf("%s in %s has no checksum (checksums.txt or %s.sha256): refused", name, s.Dir, name)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, name))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("%s in %s does not match its checksum: refused", name, s.Dir)
	}
	return nil
}

func (s *Source) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

// checksum finds name's sha256 in a `shasum -a 256` listing.
func checksum(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// extract writes the archive's tower to bin.
func extract(arch, bin string) error {
	f, err := os.Open(arch)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return errors.New("the archive has no tower binary")
		}
		if err != nil {
			return err
		}
		if filepath.Clean(h.Name) != "tower" || h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 512<<20))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}
		return writeAtomic(bin, data, 0o755)
	}
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), mode); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}
