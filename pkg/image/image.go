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

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Image struct {
	Ref        string
	Digest     string
	Tag        string // short virtiofs tag
	Rootfs     string
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
}

type Store struct {
	dir string
	mu  sync.Mutex
	inf map[string]*sync.Mutex
	mem map[string]*Image
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, inf: map[string]*sync.Mutex{}, mem: map[string]*Image{}}
}

func key(ref string) string {
	h := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(h[:8])
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
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(r, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "arm64"}))
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
	im := &Image{Ref: ref, Digest: dg.String(), Tag: "img" + key(ref), Rootfs: filepath.Join(dir, "rootfs"),
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
