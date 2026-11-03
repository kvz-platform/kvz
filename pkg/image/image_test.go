package image

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Two kvz processes on one Mac (a server and an agent) share the image store.
// Pulling the same new image from both at once must leave one complete
// rootfs, not two pulls trampling a shared temp dir. Separate Stores stand in
// for the separate processes: they share nothing but the directory.
func TestConcurrentPullsShareStore(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/img:v1"
	img, err := random.Image(4<<20, 6) // six layers
	if err != nil {
		t.Fatal(err)
	}
	r, _ := name.ParseReference(ref)
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	var want int // files in the image
	rc, _ := img.Layers()
	for range rc {
		want++
	}

	dir := filepath.Join(t.TempDir(), "images")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		s := NewStore(dir) // a fresh "process" each time
		policy := PullIfNotPresent
		if i%2 == 1 {
			policy = PullAlways // the digest-keyed path
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			im, err := s.Ensure(ref, policy, authn.DefaultKeychain)
			if err != nil {
				errs <- err
				return
			}
			n := 0
			_ = filepath.Walk(im.Rootfs, func(_ string, fi os.FileInfo, _ error) error {
				if fi != nil && fi.Mode().IsRegular() {
					n++
				}
				return nil
			})
			// random.Image writes one file per layer
			if n != want {
				errs <- fmt.Errorf("%s: %d of %d files", im.Rootfs, n, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	if len(left) > 0 {
		t.Errorf("temp dirs left behind: %v", left)
	}
}
