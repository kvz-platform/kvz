//go:build darwin

package microvmrt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/utils/exec"

	"github.com/GrowlyX/kvz/pkg/microvm"
)

// Frame types shared with kvz-init.
const (
	fStdin  = 0
	fStdout = 1
	fStderr = 2
	fExit   = 3
	fResize = 4
	fEOF    = 5
)

func writeFrame(w io.Writer, mu *sync.Mutex, t byte, p []byte) error {
	mu.Lock()
	defer mu.Unlock()
	var h [5]byte
	h[0] = t
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

func execInVM(ctx context.Context, vm *microvm.VM, container string, cmd []string, attach api.AttachIO) error {
	conn, err := vm.Dial(1025)
	if err != nil {
		return err
	}
	defer conn.Close()
	hdr, _ := json.Marshal(map[string]any{"container": container, "argv": cmd, "tty": attach.TTY(), "stdin": attach.Stdin() != nil})
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		return err
	}
	var wmu sync.Mutex
	if in := attach.Stdin(); in != nil {
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := in.Read(buf)
				if n > 0 {
					if writeFrame(conn, &wmu, fStdin, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					_ = writeFrame(conn, &wmu, fEOF, nil)
					return
				}
			}
		}()
	}
	if attach.TTY() {
		go func() {
			for sz := range attach.Resize() {
				var p [4]byte
				binary.BigEndian.PutUint16(p[0:], sz.Height)
				binary.BigEndian.PutUint16(p[2:], sz.Width)
				_ = writeFrame(conn, &wmu, fResize, p[:])
			}
		}()
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	return readFrames(conn, attach)
}

func readFrames(conn net.Conn, attach api.AttachIO) error {
	var h [5]byte
	for {
		if _, err := io.ReadFull(conn, h[:]); err != nil {
			return fmt.Errorf("exec stream closed: %w", err)
		}
		n := binary.BigEndian.Uint32(h[1:])
		p := make([]byte, n)
		if _, err := io.ReadFull(conn, p); err != nil {
			return err
		}
		switch h[0] {
		case fStdout:
			if w := attach.Stdout(); w != nil {
				_, _ = w.Write(p)
			}
		case fStderr:
			if w := attach.Stderr(); w != nil {
				_, _ = w.Write(p)
			} else if w := attach.Stdout(); w != nil {
				_, _ = w.Write(p)
			}
		case fExit:
			code := int(int32(binary.BigEndian.Uint32(p)))
			if code == 0 {
				return nil
			}
			return exec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", code), Code: code}
		}
	}
}
