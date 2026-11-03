// Package image is kvz's OCI image store: pull, flatten, and unpack an image
// once into a rootfs tree that every pod VM using it mounts read-only over
// virtiofs (the per-pod writable layer is tmpfs inside the guest). This is the
// equivalent of a containerd snapshot, not a startup cache: no per-pod work
// happens on the host at pod start.
package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

type Image struct {
	Ref         string
	Digest      string
	IndexDigest string // digest the tag resolved to (may be a multi-arch index)
	Tag         string // short virtiofs tag
	Rootfs      string
	Entrypoint  []string
	Cmd         []string
	Env         []string
	WorkingDir  string
	User        string
}

type Store struct {
	dir string
	mu  sync.Mutex
	inf map[string]*sync.Mutex
	mem map[string]*Image
	reg *Registries
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, inf: map[string]*sync.Mutex{}, mem: map[string]*Image{}}
}

func key(ref string) string {
	h := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(h[:8])
}

// Pull policies (corev1.PullPolicy values).
const (
	PullAlways       = "Always"
	PullIfNotPresent = "IfNotPresent"
	PullNever        = "Never"
)

// Ensure resolves ref under a pull policy with the given credentials.
// Images are stored per digest, so re-pulling a moving tag never swaps the
// rootfs underneath running pod VMs.
func (s *Store) Ensure(ref, policy string, kc authn.Keychain) (*Image, error) {
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	switch policy {
	case PullNever:
		if im := s.local(ref); im != nil {
			return im, nil
		}
		return nil, fmt.Errorf("ErrImageNeverPull: image %s not present and pull policy is Never", ref)
	case PullAlways:
		desc, err := s.head(ref, kc)
		if err == nil {
			if im := s.local(ref); im != nil && (im.Digest == desc.Digest.String() || im.IndexDigest == desc.Digest.String()) {
				return im, nil
			}
			return s.pullLocked(ref, kc, desc.Digest.String())
		}
		// registry unreachable: fall back to what we have (like the kubelet would fail; be lenient offline)
		if im := s.local(ref); im != nil {
			return im, nil
		}
		return nil, err
	}
	if im := s.local(ref); im != nil {
		return im, nil
	}
	return s.pullLocked(ref, kc, "")
}

// Has reports whether ref is already in the store.
func (s *Store) Has(ref string) bool { return s.local(ref) != nil }

// Dir is the store's root directory (per-digest image dirs live below it).
func (s *Store) Dir() string { return s.dir }

func (s *Store) local(ref string) *Image {
	s.mu.Lock()
	im := s.mem[ref]
	s.mu.Unlock()
	if im != nil {
		return im
	}
	dir := filepath.Join(s.dir, key(ref))
	if b, err := os.ReadFile(filepath.Join(s.dir, "refs", key(ref))); err == nil {
		dir = filepath.Join(s.dir, strings.TrimSpace(string(b)))
	}
	var m Image
	if b, err := os.ReadFile(filepath.Join(dir, "image.json")); err == nil && json.Unmarshal(b, &m) == nil {
		s.mu.Lock()
		s.mem[ref] = &m
		s.mu.Unlock()
		return &m
	}
	return nil
}

func (s *Store) pullLocked(ref string, kc authn.Keychain, digest string) (*Image, error) {
	s.mu.Lock()
	l, ok := s.inf[ref]
	if !ok {
		l = &sync.Mutex{}
		s.inf[ref] = l
	}
	s.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	if im := s.local(ref); im != nil && (digest == "" || im.Digest == digest || im.IndexDigest == digest) {
		return im, nil
	}
	dirName := key(ref + "@" + digest)
	im, err := s.pullAuth(ref, filepath.Join(s.dir, dirName), kc)
	if err != nil {
		return nil, err
	}
	im.IndexDigest = digest
	b, _ := json.MarshalIndent(im, "", " ")
	// Other kvz processes may read these at any moment: replace, don't rewrite.
	_ = writeAtomic(filepath.Join(s.dir, dirName, "image.json"), b)
	_ = os.MkdirAll(filepath.Join(s.dir, "refs"), 0o755)
	_ = writeAtomic(filepath.Join(s.dir, "refs", key(ref)), []byte(dirName))
	s.mu.Lock()
	s.mem[ref] = im
	s.mu.Unlock()
	return im, nil
}

// Get returns the unpacked image, pulling it if it is not in the store.
func (s *Store) Get(ref string) (*Image, error) {
	s.mu.Lock()
	if im, ok := s.mem[ref]; ok {
		s.mu.Unlock()
		return im, nil
	}
	l, ok := s.inf[ref]
	if !ok {
		l = &sync.Mutex{}
		s.inf[ref] = l
	}
	s.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	if im, ok := s.mem[ref]; ok {
		s.mu.Unlock()
		return im, nil
	}
	s.mu.Unlock()

	dir := filepath.Join(s.dir, key(ref))
	meta := filepath.Join(dir, "image.json")
	var im Image
	if b, err := os.ReadFile(meta); err == nil && json.Unmarshal(b, &im) == nil {
		s.mu.Lock()
		s.mem[ref] = &im
		s.mu.Unlock()
		return &im, nil
	}
	im2, err := s.pull(ref, dir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.mem[ref] = im2
	s.mu.Unlock()
	return im2, nil
}

func (s *Store) pull(ref, dir string) (*Image, error) {
	return s.pullAuth(ref, dir, authn.DefaultKeychain)
}

// pullAuth downloads ref and unpacks it into dir. Several kvz processes can
// share one store (a server and an agent on the same Mac, or two data dirs),
// so the per-process locks aren't enough: a file lock serializes pulls of the
// same dir across processes, each pull unpacks into its own temp dir, and a
// complete dir (one with image.json) is never replaced, because running pod
// VMs may have it mounted.
func (s *Store) pullAuth(ref, dir string, kc authn.Keychain) (*Image, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	unlock, err := lockFile(dir + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if im := readImage(dir); im != nil {
		return im, nil // another process finished this pull while we waited
	}
	img, err := s.image(ref, kc)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	dg, _ := img.Digest()
	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp) // no-op once renamed into place
	root := filepath.Join(tmp, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	rc := mutate.Extract(img) // flattened, whiteouts applied
	err = untar(rc, root)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", ref, err)
	}
	im := &Image{Ref: ref, Digest: dg.String(), Tag: "img" + key(dir), Rootfs: filepath.Join(dir, "rootfs"),
		Entrypoint: cf.Config.Entrypoint, Cmd: cf.Config.Cmd, Env: cf.Config.Env, WorkingDir: cf.Config.WorkingDir, User: cf.Config.User}
	b, _ := json.MarshalIndent(im, "", " ")
	if err := os.WriteFile(filepath.Join(tmp, "image.json"), b, 0o644); err != nil {
		return nil, err
	}
	// dir can only exist here without image.json: a pull that crashed before
	// this change. Nothing can be using it, so replace it.
	_ = os.RemoveAll(dir)
	return im, os.Rename(tmp, dir)
}

// readImage returns the image unpacked in dir, or nil if dir is missing or
// incomplete (image.json is written last).
func readImage(dir string) *Image {
	b, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return nil
	}
	var im Image
	if json.Unmarshal(b, &im) != nil {
		return nil
	}
	return &im
}

// writeAtomic replaces path with data so readers see the old or the new
// contents, never a partial file.
func writeAtomic(path string, data []byte) error {
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
	_ = os.Chmod(f.Name(), 0o644)
	return os.Rename(f.Name(), path)
}

// lockFile takes an exclusive flock on path, waiting for other processes.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func untar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	type link struct{ dst, target string }
	var hard []link
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p := filepath.Join(root, filepath.Clean("/"+h.Name))
		if !strings.HasPrefix(p, root) {
			continue
		}
		mode := os.FileMode(h.Mode & 0o7777)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			_ = os.Chmod(p, mode|0o700)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.Remove(p)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			_ = os.Chmod(p, mode|0o400)
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.Remove(p)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeLink:
			hard = append(hard, link{p, filepath.Join(root, filepath.Clean("/"+h.Linkname))})
		default:
			// device nodes, fifos: provided by the guest's devtmpfs
		}
	}
	for _, l := range hard {
		_ = os.MkdirAll(filepath.Dir(l.dst), 0o755)
		_ = os.Remove(l.dst)
		if err := os.Link(l.target, l.dst); err != nil {
			return err
		}
	}
	return nil
}
