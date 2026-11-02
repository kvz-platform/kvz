package runtime

import (
	"context"
	"io"
	"os"
	"time"
)

// Follow tails f until ctx is cancelled.
func Follow(ctx context.Context, f *os.File) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer f.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := pw.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err == io.EOF {
				select {
				case <-ctx.Done():
					pw.Close()
					return
				case <-time.After(250 * time.Millisecond):
				}
				continue
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}
