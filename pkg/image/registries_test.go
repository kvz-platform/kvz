package image

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
)

func TestMirrorCandidates(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "registries.yaml")
	os.WriteFile(cfg, []byte(`
mirrors:
  docker.io:
    endpoint: ["http://127.0.0.1:1/", "https://mirror.gcr.io"]
  "*":
    endpoint: ["https://catchall.example/v2"]
configs:
  mirror.gcr.io:
    auth: {username: u, password: p}
`), 0o644)
	s := NewStore(filepath.Join(dir, "images"))
	if err := s.LoadRegistries(cfg); err != nil {
		t.Fatal(err)
	}
	cs, err := s.candidates("nginx:alpine", authn.DefaultKeychain)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, c.ref.Name())
	}
	want := []string{"127.0.0.1:1/library/nginx:alpine", "mirror.gcr.io/library/nginx:alpine", "index.docker.io/library/nginx:alpine"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d: got %s want %s", i, got[i], want[i])
		}
	}
	cs, _ = s.candidates("quay.io/foo/bar@sha256:"+"0123456789012345678901234567890123456789012345678901234567890123", authn.DefaultKeychain)
	if cs[0].ref.Name() != "catchall.example/foo/bar@sha256:0123456789012345678901234567890123456789012345678901234567890123" {
		t.Fatalf("catch-all mirror: %s", cs[0].ref.Name())
	}
}

// Live pull through a mirror (skipped with -short): the first endpoint is
// dead, so this exercises fallback to mirror.gcr.io.
func TestMirrorPull(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "registries.yaml")
	os.WriteFile(cfg, []byte("mirrors:\n  docker.io:\n    endpoint: [\"http://127.0.0.1:1\", \"https://mirror.gcr.io\"]\n"), 0o644)
	s := NewStore(filepath.Join(dir, "images"))
	if err := s.LoadRegistries(cfg); err != nil {
		t.Fatal(err)
	}
	im, err := s.Ensure("busybox:1.36", PullAlways, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(im.Rootfs, "bin", "busybox")); err != nil {
		t.Fatal(err)
	}
	t.Logf("pulled %s via mirror: %s", im.Ref, im.Digest)
}
