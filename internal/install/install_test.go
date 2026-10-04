package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// localRunner runs a remote command with this machine's sh, as ssh would
// run it on a host whose home is home.
type localRunner struct {
	home  string
	env   []string
	uname string
}

func (r localRunner) Run(ctx context.Context, remote string, stdin io.Reader) (string, error) {
	if remote == "uname -s -m" && r.uname != "" {
		return r.uname + "\n", nil
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remote)
	cmd.Env = append([]string{"HOME=" + r.home, "PATH=/usr/bin:/bin"}, r.env...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, out)
	}
	return string(out), nil
}

func TestParsePlatform(t *testing.T) {
	for in, want := range map[string]string{
		"Linux x86_64": "linux/amd64", "Linux aarch64": "linux/arm64", "Darwin arm64": "darwin/arm64",
		"Darwin x86_64\n": "darwin/amd64",
	} {
		p, err := ParsePlatform(in)
		if err != nil || p.String() != want {
			t.Errorf("%q: %v %v", in, p, err)
		}
	}
	for _, bad := range []string{"FreeBSD amd64", "Linux riscv64", "", "Linux"} {
		if _, err := ParsePlatform(bad); err == nil || !strings.Contains(err.Error(), "unsupported platform") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// plat is this machine's directory name under a version: $(uname -sm).
func plat(t *testing.T) string {
	out, err := exec.Command("uname", "-sm").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(strings.TrimSpace(string(out)), " ", "-", 1)
}

// fakeTower is a binary that says it is version v, padded to about n
// bytes.
func fakeTower(v string, n int) []byte {
	b := []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo " + v + "; else echo running " + v + "; fi\nexit 0\n")
	for len(b) < n {
		b = append(b, "# padding to the size of a build\n"...)
	}
	return b
}

func upload(r localRunner, root, v string, bin []byte) error {
	_, err := r.Run(context.Background(), UploadScript(root, v, int64(len(bin))), bytes.NewReader(bin))
	return err
}

func TestPathOnTheHost(t *testing.T) {
	home := t.TempDir()
	r := localRunner{home: home}
	p := plat(t)
	echo := func(root string, env ...string) string {
		rr := r
		rr.env = env
		out, err := rr.Run(context.Background(), Locate(root, "0.0.2-dev+abc")+`; printf '%s' "$b"`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := echo(DefaultRoot); got != home+"/.local/share/tower/0.0.2-dev+abc/"+p+"/tower" {
		t.Fatalf("default root: %q", got)
	}
	for _, odd := range []string{`/tmp/a b/"q"/$x/` + "`b`", `/tmp/x}y`, `/tmp/it's`} {
		if got := echo(odd); got != odd+"/0.0.2-dev+abc/"+p+"/tower" {
			t.Fatalf("a root with odd characters %q: %q", odd, got)
		}
	}
	if got := echo(DefaultRoot, "TOWER_INSTALL_DIR=/elsewhere"); got != "/elsewhere/0.0.2-dev+abc/"+p+"/tower" {
		t.Fatalf("the host's own TOWER_INSTALL_DIR: %q", got)
	}
	// A missing build is 127 in every sh (exec of a missing file is 126
	// in macOS's).
	if _, err := r.Run(context.Background(), Command("~/none", "1.0", "version"), nil); err == nil || !strings.Contains(err.Error(), "exit status 127") {
		t.Fatalf("a missing build: %v", err)
	}
	// The arguments survive, through the platform directory.
	bin := fakeTower("1.0", 0)
	if err := upload(r, "~/x", "1.0", bin); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "x", "1.0", p, "tower"), []byte("#!/bin/sh\nprintf '%s|' \"$@\"\n"), 0o755)
	out, _ := r.Run(context.Background(), Command("~/x", "1.0", "towerd", "--tmux", "-L a b", "it's"), nil)
	if out != "towerd|--tmux|-L a b|it's|" {
		t.Fatalf("arguments: %q", out)
	}
}

func TestUploadIsAtomicAndSwapsCurrent(t *testing.T) {
	home := t.TempDir()
	r := localRunner{home: home}
	p := plat(t)
	bin := fakeTower("0.1.0", 2_600_000)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- upload(r, "~/t", "0.1.0", bin)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(home, "t", "0.1.0", p, "tower")
	got, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("four installs at once left %d bytes (want %d): %v", len(got), len(bin), err)
	}
	if st, _ := os.Stat(installed); st.Mode()&0o111 == 0 {
		t.Fatal("not executable")
	}
	if l, _ := os.Readlink(filepath.Join(home, "t", "current")); l != "0.1.0/"+p {
		t.Fatalf("current → %q", l)
	}
	if err := upload(r, "~/t", "0.2.0", fakeTower("0.2.0", 100)); err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(home, "t", "current")); l != "0.2.0/"+p {
		t.Fatalf("current after a newer install → %q", l)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatal("the older build went")
	}
	// An older version installed later (a home not upgraded yet) leaves
	// current on the newer one.
	if err := upload(r, "~/t", "0.1.5", fakeTower("0.1.5", 100)); err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(home, "t", "current")); l != "0.2.0/"+p {
		t.Fatalf("an older install moved current to %q", l)
	}
	left, _ := filepath.Glob(filepath.Join(home, "t", ".current.*"))
	tmp, _ := filepath.Glob(filepath.Join(home, "t", "*", "*", ".tower.*"))
	if len(left)+len(tmp) != 0 {
		t.Fatalf("temporary files left: %v %v", left, tmp)
	}
}

// An upload cut short, or a binary that is not the version it is
// installed as, is refused: nothing goes in place.
func TestUploadRefusesABadBuild(t *testing.T) {
	home := t.TempDir()
	r := localRunner{home: home}
	p := plat(t)
	bin := fakeTower("0.3.0", 100_000)
	_, err := r.Run(context.Background(), UploadScript("~/t", "0.3.0", int64(len(bin))), bytes.NewReader(bin[:len(bin)/2]))
	if err == nil || !strings.Contains(err.Error(), "upload cut short") {
		t.Fatalf("a short upload: %v", err)
	}
	if err := upload(r, "~/t", "0.3.0", fakeTower("0.4.0", 100)); err == nil || !strings.Contains(err.Error(), "not 0.3.0") {
		t.Fatalf("a build of another version: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "t", "0.3.0", p, "tower")); err == nil {
		t.Fatal("a refused build was put in place")
	}
	if _, err := os.Lstat(filepath.Join(home, "t", "current")); err == nil {
		t.Fatal("current points at a refused build")
	}
	tmp, _ := filepath.Glob(filepath.Join(home, "t", "*", "*", ".tower.*"))
	if len(tmp) != 0 {
		t.Fatalf("temporary files left: %v", tmp)
	}
}

// archive is a release archive holding a tower binary with content body.
func archive(t *testing.T, body string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "tower", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sumLine(data []byte, name string) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:]) + "  " + name + "\n"
}

// other is a platform that is not this one.
func other() Platform {
	if Local().OS == "linux" {
		return Platform{"darwin", "arm64"}
	}
	return Platform{"linux", "amd64"}
}

func TestSourceThisPlatformIsThisBinary(t *testing.T) {
	s := &Source{Version: "1.0.0", Self: "/the/self", Dir: t.TempDir()}
	if b, err := s.Binary(context.Background(), Local()); err != nil || b != "/the/self" {
		t.Fatal(b, err)
	}
}

func TestSourceFromTheCache(t *testing.T) {
	dir := t.TempDir()
	p := other()
	name := Archive("1.0.0-dev+abc", p)
	data := archive(t, "cached build")
	os.WriteFile(filepath.Join(dir, name), data, 0o644)
	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sumLine(data, name)), 0o644)
	s := &Source{Version: "1.0.0-dev+abc", Dir: dir, ReleaseURL: "http://127.0.0.1:1"}
	b, err := s.Binary(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b); string(got) != "cached build" {
		t.Fatalf("%q", got)
	}
	// A cached archive that no longer matches its checksum is refused.
	os.WriteFile(filepath.Join(dir, name), archive(t, "tampered"), 0o644)
	if _, err := s.Binary(context.Background(), p); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a tampered cache: %v", err)
	}
	// So is one that no checksum lists.
	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sumLine(data, "something-else.tar.gz")), 0o644)
	os.WriteFile(filepath.Join(dir, name), data, 0o644)
	if _, err := s.Binary(context.Background(), p); err == nil || !strings.Contains(err.Error(), "has no checksum") {
		t.Fatalf("an unlisted archive: %v", err)
	}
}

func TestSourceDevBuildNeedsTheCache(t *testing.T) {
	s := &Source{Version: "0.0.3-dev+abc", Dir: t.TempDir(), ReleaseURL: "http://127.0.0.1:1"}
	_, err := s.Binary(context.Background(), other())
	if err == nil || !strings.Contains(err.Error(), "no tower 0.0.3-dev+abc build for "+other().String()+": run mise run dist") {
		t.Fatalf("%v", err)
	}
}

func TestSourceDownloadsAndVerifies(t *testing.T) {
	p := other()
	good := archive(t, "released build")
	name := Archive("1.2.3", p)
	sums := sumLine(good, name)
	serve := good
	var hits sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Store(r.URL.Path, true)
		switch r.URL.Path {
		case "/v1.2.3/checksums.txt":
			io.WriteString(w, sums)
		case "/v1.2.3/" + name:
			w.Write(serve)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	// A cache filled by mise run dist: its checksums.txt lists a local
	// build, which a download must leave alone.
	local := archive(t, "built here")
	localName := Archive("1.2.3", Local())
	os.WriteFile(filepath.Join(dir, localName), local, 0o644)
	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sumLine(local, localName)), 0o644)
	s := &Source{Version: "1.2.3", Dir: dir, ReleaseURL: srv.URL, HTTP: srv.Client()}

	// A bad checksum is refused, and nothing is kept.
	serve = archive(t, "corrupted")
	if _, err := s.Binary(context.Background(), p); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a bad download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Fatal("a bad download was kept")
	}
	serve = good
	b, err := s.Binary(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b); string(got) != "released build" {
		t.Fatalf("%q", got)
	}
	if err := s.verifyCached(localName); err != nil {
		t.Fatalf("a download broke the local build's checksum: %v", err)
	}
	// Kept: the next install needs no network.
	srv.Close()
	if _, err := s.Binary(context.Background(), p); err != nil {
		t.Fatalf("from the cache after a download: %v", err)
	}
	// An asset the release lacks.
	s2 := &Source{Version: "1.2.3", Dir: t.TempDir(), ReleaseURL: "http://127.0.0.1:1", HTTP: srv.Client()}
	if _, err := s2.Binary(context.Background(), p); err == nil || !strings.Contains(err.Error(), "cannot download tower 1.2.3") {
		t.Fatalf("no server: %v", err)
	}
}

func TestInstallerInstalls(t *testing.T) {
	home := t.TempDir()
	self := filepath.Join(t.TempDir(), "tower")
	os.WriteFile(self, fakeTower("0.5.0", 0), 0o755)
	in := &Installer{Version: "0.5.0", Root: "~/r", Source: &Source{Version: "0.5.0", Self: self, Dir: t.TempDir()}}
	r := localRunner{home: home, uname: map[string]string{"darwin/arm64": "Darwin arm64", "darwin/amd64": "Darwin x86_64", "linux/amd64": "Linux x86_64", "linux/arm64": "Linux aarch64"}[Local().String()]}
	if err := in.Install(context.Background(), r, "B"); err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(context.Background(), in.Command(), nil)
	if err != nil || out != "running 0.5.0\n" {
		t.Fatalf("the installed build: %q %v", out, err)
	}
	r.uname = "SunOS sparc"
	if err := in.Install(context.Background(), r, "B"); err == nil || err.Error() != `cannot install tower on B: unsupported platform SunOS sparc` {
		t.Fatalf("%v", err)
	}
}
