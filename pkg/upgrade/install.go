package upgrade

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GrowlyX/kvz/pkg/version"
)

// maxBootAttempts is how many times a freshly installed binary may start
// without its node becoming Ready before it is rolled back.
const maxBootAttempts = 3

// maxDownload caps a release tarball (binary + kernel are ~150 MB).
const maxDownload = 2 << 30

// state is <data-dir>/upgrade/state.json: an install that hasn't been
// confirmed yet, or the version that was rolled back.
type state struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	Exe      string    `json:"exe"`
	Files    []string  `json:"files"` // installed paths; <path>.prev is the old file
	Attempts int       `json:"attempts"`
	Failed   string    `json:"failed,omitempty"` // version that was rolled back
	Reason   string    `json:"reason,omitempty"`
	Time     time.Time `json:"time"`
}

func stateDir(dataDir string) string  { return filepath.Join(dataDir, "upgrade") }
func statePath(dataDir string) string { return filepath.Join(stateDir(dataDir), "state.json") }

func loadState(dataDir string) *state {
	b, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return nil
	}
	var s state
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func saveState(dataDir string, s *state) error {
	if err := os.MkdirAll(stateDir(dataDir), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := statePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dataDir))
}

// BootCheck runs first thing in `kvz server` and `kvz agent`. A binary that
// was just installed by an upgrade counts its starts; once it has started
// maxBootAttempts times without its node becoming Ready, the previous files
// are put back and BootCheck returns the binary to exec instead.
func BootCheck(dataDir string) (rollbackTo string, err error) {
	s := loadState(dataDir)
	if s == nil || s.Failed != "" || s.To != version.Release {
		return "", nil
	}
	s.Attempts++
	if s.Attempts <= maxBootAttempts {
		return "", saveState(dataDir, s)
	}
	for _, f := range s.Files {
		if _, err := os.Stat(f + ".prev"); err != nil {
			return "", fmt.Errorf("rollback: %s.prev: %w", f, err)
		}
		if err := os.Rename(f+".prev", f); err != nil {
			return "", fmt.Errorf("rollback %s: %w", f, err)
		}
	}
	s.Failed = s.To
	s.Reason = fmt.Sprintf("%s started %d times without its node becoming Ready; rolled back to %s", s.To, maxBootAttempts, s.From)
	if err := saveState(dataDir, s); err != nil {
		return "", err
	}
	return s.Exe, nil
}

// download fetches url into dst and returns its sha256.
func download(ctx context.Context, hc *http.Client, url, dst string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", err
	}
	if n > maxDownload {
		return "", fmt.Errorf("GET %s: larger than %d bytes", url, maxDownload)
	}
	return hex.EncodeToString(h.Sum(nil)), f.Close()
}

// fetchChecksum reads a `shasum -a 256` line from url.
func fetchChecksum(ctx context.Context, hc *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return parseChecksum(b)
}

func parseChecksum(b []byte) (string, error) {
	f := strings.Fields(string(b))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", fmt.Errorf("bad checksum file")
	}
	if _, err := hex.DecodeString(f[0]); err != nil {
		return "", fmt.Errorf("bad checksum file")
	}
	return strings.ToLower(f[0]), nil
}

// releaseFiles are the tarball members kvz installs, relative to the prefix
// (the tarball's top directory; see hack/release/package.sh).
var releaseFiles = map[string]bool{
	"bin/kvz":                 true,
	"share/kvz/kernel/Image":  true,
	"share/kvz/kernel/config": true,
}

// extract unpacks the release files of tarball into dir and returns the
// relative paths it wrote.
func extract(tarball, dir string) ([]string, error) {
	f, err := os.Open(tarball)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	var got []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// strip the top-level kvz-<version>-darwin-arm64/ directory
		name := filepath.ToSlash(filepath.Clean(h.Name))
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if !releaseFiles[name] {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, h.FileInfo().Mode().Perm()|0o400)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxDownload)); err != nil {
			out.Close()
			return nil, err
		}
		if err := out.Close(); err != nil {
			return nil, err
		}
		got = append(got, name)
	}
	if !contains(got, "bin/kvz") {
		return nil, fmt.Errorf("tarball has no bin/kvz")
	}
	return got, nil
}

// verifyBinary checks a candidate kvz binary before it replaces the running
// one: a valid code signature with the virtualization entitlement, the same
// signing team as the running binary (when that one is Developer ID signed),
// and that it runs and reports the expected release.
func verifyBinary(ctx context.Context, bin, running, want string) error {
	if out, err := exec.CommandContext(ctx, "codesign", "--verify", "--strict", bin).CombinedOutput(); err != nil {
		return fmt.Errorf("code signature: %v: %s", err, bytes.TrimSpace(out))
	}
	ent, err := exec.CommandContext(ctx, "codesign", "-d", "--entitlements", "-", "--xml", bin).Output()
	if err != nil || !bytes.Contains(ent, []byte("com.apple.security.virtualization")) {
		return fmt.Errorf("binary lacks the com.apple.security.virtualization entitlement")
	}
	if team := teamID(ctx, running); team != "" {
		if got := teamID(ctx, bin); got != team {
			return fmt.Errorf("binary is signed by team %q, running kvz by %q", got, team)
		}
	}
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, bin, "version").Output()
	if err != nil {
		return fmt.Errorf("%s version: %w", bin, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "kvz" || f[1] != want {
		return fmt.Errorf("binary reports %q, want kvz %s", strings.TrimSpace(string(out)), want)
	}
	return nil
}

// teamID is the signing team of a binary, "" for ad-hoc or unsigned ones.
func teamID(ctx context.Context, bin string) string {
	out, _ := exec.CommandContext(ctx, "codesign", "-dv", bin).CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "TeamIdentifier="); ok && v != "not set" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// installTarget maps tarball members to the paths they replace: the binary
// itself and, for packaged installs (Homebrew, release tarballs), the kernel
// at <prefix>/share/kvz/kernel. Source builds keep their ~/.kvz/kernel.
func installTarget(exe string, members []string) map[string]string {
	prefix := filepath.Dir(filepath.Dir(exe))
	kdir := filepath.Join(prefix, "share", "kvz", "kernel")
	_, kerr := os.Stat(kdir)
	m := map[string]string{}
	for _, rel := range members {
		switch {
		case rel == "bin/kvz":
			m[rel] = exe
		case strings.HasPrefix(rel, "share/kvz/kernel/") && kerr == nil:
			m[rel] = filepath.Join(prefix, filepath.FromSlash(rel))
		}
	}
	return m
}

// swap installs src at dst, keeping the old file as dst.prev. The final
// rename is atomic, and the running process keeps its (unlinked) image.
func swap(src, dst string) error {
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(dst + ".prev")
		if err := os.Link(dst, dst+".prev"); err != nil {
			if err := copyFile(dst, dst+".prev"); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
