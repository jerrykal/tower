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

func TestPathOnTheHost(t *testing.T) {
	home := t.TempDir()
	r := localRunner{home: home}
	echo := func(root string, env ...string) string {
		rr := r
		rr.env = env
		out, err := rr.Run(context.Background(), `printf '%s' `+Path(root, "0.0.2-dev+abc"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := echo(DefaultRoot); got != home+"/.local/share/tower/0.0.2-dev+abc/tower" {
		t.Fatalf("default root: %q", got)
	}
	odd := `/tmp/a b/"q"/$x/` + "`b`"
	if got := echo(odd); got != odd+"/0.0.2-dev+abc/tower" {
		t.Fatalf("a root with odd characters: %q", got)
	}
	if got := echo(DefaultRoot, "TOWER_INSTALL_DIR=/elsewhere"); got != "/elsewhere/0.0.2-dev+abc/tower" {
		t.Fatalf("the host's own TOWER_INSTALL_DIR: %q", got)
	}
	cmd := Command("~/x", "1.0", "towerd", "--tmux", "-L a b")
	out, _ := localRunner{home: home}.Run(context.Background(), "set -- "+strings.TrimPrefix(cmd, Path("~/x", "1.0"))+`; printf '%s|' "$@"`, nil)
	if out != "towerd|--tmux|-L a b|" {
		t.Fatalf("arguments: %q", out)
	}
}

func TestUploadIsAtomicAndSwapsCurrent(t *testing.T) {
	home := t.TempDir()
	r := localRunner{home: home}
	ctx := context.Background()
	bin := bytes.Repeat([]byte("tower-binary\n"), 200_000) // 2.6 MB
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Run(ctx, UploadScript("~/t", "0.1.0"), bytes.NewReader(bin))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.Join(home, "t", "0.1.0", "tower"))
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("four installs at once left %d bytes (want %d): %v", len(got), len(bin), err)
	}
	if st, _ := os.Stat(filepath.Join(home, "t", "0.1.0", "tower")); st.Mode()&0o111 == 0 {
		t.Fatal("not executable")
	}
	if l, _ := os.Readlink(filepath.Join(home, "t", "current")); l != "0.1.0" {
		t.Fatalf("current → %q", l)
	}
	if _, err := r.Run(ctx, UploadScript("~/t", "0.2.0"), strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(home, "t", "current")); l != "0.2.0" {
		t.Fatalf("current after a newer install → %q", l)
	}
	if _, err := os.Stat(filepath.Join(home, "t", "0.1.0", "tower")); err != nil {
		t.Fatal("the older build went")
	}
	left, _ := filepath.Glob(filepath.Join(home, "t", ".current.*"))
	tmp, _ := filepath.Glob(filepath.Join(home, "t", "*", ".tower.*"))
	if len(left)+len(tmp) != 0 {
		t.Fatalf("temporary files left: %v %v", left, tmp)
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
	os.WriteFile(self, []byte("#!/bin/sh\necho self\n"), 0o755)
	in := &Installer{Version: "0.5.0", Root: "~/r", Source: &Source{Version: "0.5.0", Self: self, Dir: t.TempDir()}}
	r := localRunner{home: home, uname: map[string]string{"darwin/arm64": "Darwin arm64", "darwin/amd64": "Darwin x86_64", "linux/amd64": "Linux x86_64", "linux/arm64": "Linux aarch64"}[Local().String()]}
	if err := in.Install(context.Background(), r, "B"); err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(context.Background(), in.Command(), nil)
	if err != nil || out != "self\n" {
		t.Fatalf("the installed build: %q %v", out, err)
	}
	r.uname = "SunOS sparc"
	if err := in.Install(context.Background(), r, "B"); err == nil || err.Error() != `cannot install tower on B: unsupported platform SunOS sparc` {
		t.Fatalf("%v", err)
	}
}
