package microvm

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// cpio "newc" writer, just enough for an initramfs.
type cpio struct {
	b   bytes.Buffer
	ino int
}

func (c *cpio) entry(name string, mode uint32, data []byte, rdevMajor, rdevMinor int) {
	c.ino++
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.ino, mode, 0, 0, 1, 0, len(data), 0, 0, rdevMajor, rdevMinor, len(name)+1, 0)
	c.b.WriteString(hdr)
	c.b.WriteString(name)
	c.b.WriteByte(0)
	c.pad()
	c.b.Write(data)
	c.pad()
}

func (c *cpio) pad() {
	for c.b.Len()%4 != 0 {
		c.b.WriteByte(0)
	}
}

const (
	sIFDIR = 0o040000
	sIFREG = 0o100000
	sIFCHR = 0o020000
)

// BuildInitrd returns an uncompressed initramfs containing /init (the kvz
// agent) and the mount points it needs. Uncompressed on purpose: the kernel
// unpacks it with a memcpy instead of a gunzip.
func BuildInitrd(initBin []byte) []byte {
	c := &cpio{}
	for _, d := range []string{"dev", "proc", "sys", "kvz", "img", "rw", "c", "tmp"} {
		c.entry(d, sIFDIR|0o755, nil, 0, 0)
	}
	c.entry("dev/console", sIFCHR|0o600, nil, 5, 1)
	c.entry("dev/null", sIFCHR|0o666, nil, 1, 3)
	c.entry("init", sIFREG|0o755, initBin, 0, 0)
	c.entry("TRAILER!!!", 0, nil, 0, 0)
	return c.b.Bytes()
}

var (
	initrdOnce sync.Once
	initrdPath string
	initrdErr  error
)

// InitrdPath writes the initramfs once per process and returns its path.
func InitrdPath(dir string, initBin []byte) (string, error) {
	initrdOnce.Do(func() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			initrdErr = err
			return
		}
		initrdPath = dir + "/kvz-initrd.cpio"
		initrdErr = os.WriteFile(initrdPath, BuildInitrd(initBin), 0o644)
	})
	return initrdPath, initrdErr
}
