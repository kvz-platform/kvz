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
	_ = os.WriteFile(filepath.Join(s.dir, dirName, "image.json"), b, 0o644)
	_ = os.MkdirAll(filepath.Join(s.dir, "refs"), 0o755)
	_ = os.WriteFile(filepath.Join(s.dir, "refs", key(ref)), []byte(dirName), 0o644)
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

func (s *Store) pullAuth(ref, dir string, kc authn.Keychain) (*Image, error) {
	img, err := s.image(ref, kc)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	dg, _ := img.Digest()
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	root := filepath.Join(tmp, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	rc := mutate.Extract(img) // flattened, whiteouts applied
	err = untar(rc, root)
	rc.Close()
	if err != nil {
		return nil, err
	}
	im := &Image{Ref: ref, Digest: dg.String(), Tag: "img" + key(dir), Rootfs: filepath.Join(dir, "rootfs"),
		Entrypoint: cf.Config.Entrypoint, Cmd: cf.Config.Cmd, Env: cf.Config.Env, WorkingDir: cf.Config.WorkingDir, User: cf.Config.User}
	b, _ := json.MarshalIndent(im, "", " ")
	if err := os.WriteFile(filepath.Join(tmp, "image.json"), b, 0o644); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(dir)
	return im, os.Rename(tmp, dir)
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
