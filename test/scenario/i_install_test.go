package scenario

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// uploads counts the installs to host in the ssh log: sessions whose
// command writes the binary from stdin.
func (w *World) uploads(host string) int {
	n := 0
	for _, c := range w.SSHLog() {
		if cmd, _ := c["cmd"].(string); c["host"] == host && strings.Contains(cmd, `cat > "$t"`) {
			n++
		}
	}
	return n
}

// installed checks that the host's install root holds version's whole
// binary (the built one) and that current points at want.
func installed(t *testing.T, h *Host, version, want string) {
	t.Helper()
	bin := filepath.Join(h.InstallDir(), version, h.PlatformDir(), "tower")
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("%s has no %s build: %v", h.Name, version, err)
	}
	src := towerBin
	if version == Version2 {
		src = tower2
	}
	if wantB, _ := os.ReadFile(src); !bytes.Equal(got, wantB) {
		t.Fatalf("%s's %s build is not whole: %d bytes, want %d", h.Name, version, len(got), len(wantB))
	}
	if st, _ := os.Stat(bin); st.Mode()&0o111 == 0 {
		t.Fatalf("%s's %s build is not executable", h.Name, version)
	}
	if cur, err := os.Readlink(filepath.Join(h.InstallDir(), "current")); err != nil || cur != want+"/"+h.PlatformDir() {
		t.Fatalf("%s's current → %q (%v), want %s", h.Name, cur, err, want)
	}
	left, _ := filepath.Glob(filepath.Join(h.InstallDir(), ".current.*"))
	tmp, _ := filepath.Glob(filepath.Join(h.InstallDir(), "*", "*", ".tower.*"))
	if len(left)+len(tmp) > 0 {
		t.Fatalf("temporary files left on %s: %v %v", h.Name, left, tmp)
	}
}

// I01: a host without tower: the first connect installs this build under
// TOWER_INSTALL_DIR, swaps current, and the host is up; the next connect
// does not install again.
func TestI01(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "i01")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	start := time.Now()
	w.Home(a, b.Unpinned())
	w.WaitLink(a, "B", "up", 15*time.Second)
	t.Logf("B installed and up after %v", time.Since(start).Round(time.Millisecond))
	installed(t, b, Version, Version)
	if n := w.uploads("B"); n != 1 {
		t.Fatalf("%d uploads", n)
	}
	if warn := w.Link(a, "B").Warn; !strings.Contains(warn, "installed "+Version) {
		t.Fatalf("B's warning: %q", warn)
	}
	streams := 0
	for _, c := range w.SSHLog() {
		if cmd, _ := c["cmd"].(string); c["host"] == "B" && strings.Contains(cmd, "/"+Version+"/") && strings.Contains(cmd, `exec "$b" towerd --stdio`) {
			streams++
		}
	}
	if streams == 0 {
		t.Fatal("the stream did not run the versioned build")
	}
	gen := w.Link(a, "B").Link
	if err := a.Call(proto.CallWake, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.Eventually(6*time.Second, "B up again", func() bool {
		l := w.Link(a, "B")
		return l.Status == "up" && l.Link > gen
	})
	if n := w.uploads("B"); n != 1 {
		t.Fatalf("the second connect installed again (%d uploads)", n)
	}

	// tower host add installs as its last check.
	c := w.Host("C", []string{"charlie"}, SSHHost())
	out, err := a.Tower("host", "add", "C", "--tmux", "-L "+c.Sock)
	t.Logf("host add:\n%s", out)
	if err != nil || !strings.Contains(out, "✓ tower: installed "+Version) {
		t.Fatalf("host add: %v", err)
	}
	installed(t, c, Version, Version)
	w.WaitLink(a, "C", "up", 10*time.Second)
	if n := w.uploads("C"); n != 1 {
		t.Fatalf("%d uploads to C", n)
	}
	out, _ = a.Tower("host", "add", "C", "--name", "c2", "--tmux", "-L "+c.Sock)
	if !strings.Contains(out, "✓ tower: "+Version) || w.uploads("C") != 1 {
		t.Fatalf("a second host add of C:\n%s", out)
	}
}

// I02: the home upgraded: the new version is installed beside the old,
// current swapped, the older towerd replaced by the new bridge, and a
// loop's client there kept.
func TestI02(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "i02")
	a := w.Host("A", []string{"alpha"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	w.Home(a, b.Unpinned())
	w.WaitLink(a, "B", "up", 15*time.Second)
	l := w.FakeLoop(a)
	l.Attach(w.Ref(a, "B", "bravo"), b)
	old := b.TowerdPid()
	start := time.Now()
	if out, err := a.TowerBin(tower2, "_ensure"); err != nil {
		t.Fatal(err, out)
	}
	w.Eventually(15*time.Second, "B up on the new version", func() bool {
		ls := w.Link(a, "B")
		return ls.Status == "up" && ls.Version == Version2
	})
	t.Logf("B on %s %v after the home's upgrade", Version2, time.Since(start).Round(time.Millisecond))
	installed(t, b, Version, Version2)
	installed(t, b, Version2, Version2)
	if pid := b.TowerdPid(); pid == old || pid == 0 || Alive(old) {
		t.Fatalf("B's towerd %d → %d (old alive %v)", old, pid, Alive(old))
	}
	if got := b.Clients(); !slices.Equal(got, []string{"bravo"}) {
		t.Fatalf("B's clients after the upgrade: %v", got)
	}
	if regs := b.Regs(); len(regs) != 1 || regs[0].Name == "" {
		t.Fatalf("B's registrations: %+v", regs)
	}
	w.WaitLoop(a, "^B:bravo", 8*time.Second)
}

// I03: a pinned host is never installed to; a missing pinned binary is
// failed: tower is not installed.
func TestI03(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "i03")
	a := w.Host("A", []string{"alpha"})
	p := w.Host("P", []string{"papa"}, SSHHost())
	q := w.Host("Q", []string{"quebec"}, SSHHost())
	qr := q.Remote()
	qr.Tower = "/nonexistent/tower"
	w.Home(a, p.Remote(), qr)
	w.WaitLink(a, "P", "up", 10*time.Second)
	w.WaitLink(a, "Q", "failed", 10*time.Second)
	if l := w.Link(a, "Q"); !strings.Contains(l.Reason, "tower is not installed on Q") {
		t.Fatalf("Q: %q", l.Reason)
	}
	time.Sleep(time.Second)
	for _, h := range []*Host{p, q} {
		if n := w.uploads(h.Name); n != 0 {
			t.Fatalf("%d uploads to pinned %s", n, h.Name)
		}
		if _, err := os.Stat(h.InstallDir()); err == nil {
			t.Fatalf("%s's install root exists", h.Name)
		}
	}
}

// unameOf is what uname -s -m says on a platform.
var unameOf = map[string]string{
	"linux/amd64": "Linux x86_64", "linux/arm64": "Linux aarch64",
	"darwin/amd64": "Darwin x86_64", "darwin/arm64": "Darwin arm64",
}

// otherPlatforms are the supported platforms but this one.
func otherPlatforms() []string {
	var out []string
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		if p != runtime.GOOS+"/"+runtime.GOARCH {
			out = append(out, p)
		}
	}
	return out
}

// releaseArchive is a release archive for a simulated platform: it holds
// the built tower (the simulated host is this machine).
func releaseArchive(t *testing.T) []byte {
	bin, err := os.ReadFile(towerBin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "tower", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	tw.Write(bin)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func archiveName(version, platform string) string {
	os, arch, _ := strings.Cut(platform, "/")
	return "tower_" + version + "_" + os + "_" + arch + ".tar.gz"
}

// I04: another platform: the build comes from the dist cache; missing
// there, from the release server with its checksum verified; a bad
// checksum is refused; a development build with no cache fails with the
// mise run dist fix.
func TestI04(t *testing.T) {
	parallel(t)
	plats := otherPlatforms()
	data := releaseArchive(t)
	cached, released, tampered := plats[0], plats[1], plats[2]
	t.Run("cache-release-checksum", func(t *testing.T) {
		w := NewWorld(t, "i04")
		dist := filepath.Join(w.Dir, "dist")
		vdir := filepath.Join(dist, Version)
		os.MkdirAll(vdir, 0o755)
		os.WriteFile(filepath.Join(vdir, archiveName(Version, cached)), data, 0o644)
		os.WriteFile(filepath.Join(vdir, "checksums.txt"), []byte(sha(data)+"  "+archiveName(Version, cached)+"\n"), 0o644)
		bad := append([]byte(nil), data...)
		bad[len(bad)/2] ^= 0xff
		var sums strings.Builder
		sums.WriteString(sha(data) + "  " + archiveName(Version, released) + "\n")
		sums.WriteString(sha(data) + "  " + archiveName(Version, tampered) + "\n")
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v" + Version + "/checksums.txt":
				rw.Write([]byte(sums.String()))
			case "/v" + Version + "/" + archiveName(Version, released):
				rw.Write(data)
			case "/v" + Version + "/" + archiveName(Version, tampered):
				rw.Write(bad)
			default:
				http.NotFound(rw, r)
			}
		}))
		defer srv.Close()
		a := w.Host("A", []string{"alpha"}, Env("TOWER_DIST_DIR", dist), Env("TOWER_RELEASE_URL", srv.URL))
		c := w.Host("C", []string{"charlie"}, Platform(unameOf[cached]), SSHHost())
		r := w.Host("R", []string{"romeo"}, Platform(unameOf[released]), SSHHost())
		x := w.Host("X", []string{"xray"}, Platform(unameOf[tampered]), SSHHost())
		w.Home(a, c.Unpinned(), r.Unpinned(), x.Unpinned())
		w.WaitLink(a, "C", "up", 15*time.Second)
		installed(t, c, Version, Version)
		w.WaitLink(a, "R", "up", 15*time.Second)
		installed(t, r, Version, Version)
		if b, err := os.ReadFile(filepath.Join(vdir, archiveName(Version, released))); err != nil || !bytes.Equal(b, data) {
			t.Fatal("the downloaded archive was not kept in the dist cache")
		}
		w.WaitLink(a, "X", "failed", 15*time.Second)
		reason := w.Link(a, "X").Reason
		t.Logf("from the cache: %s; downloaded: %s; tampered: %s → %s", cached, released, tampered, reason)
		if !strings.Contains(reason, "checksum mismatch") {
			t.Fatalf("X: %q", reason)
		}
		if _, err := os.Stat(filepath.Join(x.InstallDir(), Version)); err == nil {
			t.Fatal("a build that failed its checksum was installed")
		}
		if _, err := os.Stat(filepath.Join(vdir, archiveName(Version, tampered))); err == nil {
			t.Fatal("a download that failed its checksum was kept")
		}
	})
	t.Run("dev", func(t *testing.T) {
		w := NewWorld(t, "i04-dev")
		a := w.Host("A", []string{"alpha"})
		l := w.Host("L", []string{"lima"}, Platform(unameOf[cached]), SSHHost())
		if err := config.SaveHosts(a.Paths().HostsFile(), []config.Host{l.Unpinned()}); err != nil {
			t.Fatal(err)
		}
		out, err := a.TowerBin(towerDev, "_ensure")
		if err != nil {
			t.Fatal(err, out)
		}
		var st proto.Status
		json.Unmarshal([]byte(out), &st)
		if st.Version != VersionDev {
			t.Fatalf("home on %q", st.Version)
		}
		w.WaitLink(a, "L", "failed", 15*time.Second)
		want := "no tower " + VersionDev + " build for " + cached + ": run mise run dist"
		if reason := w.Link(a, "L").Reason; !strings.Contains(reason, want) {
			t.Fatalf("L: %q, want %q", reason, want)
		}
	})
}

// I05: two homes installing on one host at once leave one whole binary
// and a valid current.
func TestI05(t *testing.T) {
	parallel(t)
	w := NewWorld(t, "i05")
	a1 := w.Host("A1", []string{"one"})
	a2 := w.Host("A2", []string{"two"})
	b := w.Host("B", []string{"bravo"}, SSHHost())
	for _, h := range []*Host{a1, a2} {
		if err := config.SaveHosts(h.Paths().HostsFile(), []config.Host{b.Unpinned()}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan *exec.Cmd, 2)
	for _, h := range []*Host{a1, a2} {
		go func() {
			cmd := exec.Command(towerBin, "towerd")
			cmd.Env = h.Env()
			cmd.Start()
			done <- cmd
		}()
	}
	for range 2 {
		c := <-done
		w.spawned = append(w.spawned, c)
		go c.Wait()
	}
	w.WaitLink(a1, "B", "up", 20*time.Second)
	w.WaitLink(a2, "B", "up", 20*time.Second)
	t.Logf("%d uploads from two homes at once", w.uploads("B"))
	installed(t, b, Version, Version)
	w.Eventually(3*time.Second, "two homes on B", func() bool { return len(b.LiveHomes()) == 2 })
}
