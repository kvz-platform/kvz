//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	fStdin  = 0
	fStdout = 1
	fStderr = 2
	fExit   = 3
	fResize = 4
	fEOF    = 5
)

var specByName = map[string]Container{}

// serveExec accepts exec sessions from the host on vsock port 1025.
func serveExec() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: 1025}); err != nil {
		return
	}
	if err := unix.Listen(fd, 16); err != nil {
		return
	}
	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			continue
		}
		go handleExec(os.NewFile(uintptr(nfd), "exec"))
	}
}

type frameWriter struct {
	w  io.Writer
	mu *sync.Mutex
	t  byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var h [5]byte
	h[0] = f.t
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := f.w.Write(h[:]); err != nil {
		return 0, err
	}
	return f.w.Write(p)
}

func handleExec(conn *os.File) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Container string   `json:"container"`
		Argv      []string `json:"argv"`
		TTY       bool     `json:"tty"`
		Stdin     bool     `json:"stdin"`
	}
	if json.Unmarshal(line, &req) != nil || len(req.Argv) == 0 {
		return
	}
	var mu sync.Mutex
	stdout := frameWriter{conn, &mu, fStdout}
	stderr := frameWriter{conn, &mu, fStderr}
	if req.Argv[0] == "__kvz_stats" {
		stdout.Write(statsJSON())
		sendExit(conn, &mu, 0)
		return
	}
	c, ok := specByName[req.Container]
	if !ok {
		for _, v := range specByName {
			c = v
			break
		}
	}
	cmd, err := containerCmd(c, req.Argv)
	if err != nil {
		stderr.Write([]byte(err.Error() + "\n"))
		sendExit(conn, &mu, 127)
		return
	}
	var stdinW io.WriteCloser
	var ptmx *os.File
	var ch chan int
	var copies sync.WaitGroup
	if req.TTY {
		cmd.SysProcAttr.Setsid = true
		cmd.SysProcAttr.Setctty = true
		pmu.Lock()
		ptmx, err = pty.Start(cmd)
		if err == nil {
			ch = registerWaiter(cmd.Process.Pid)
		}
		pmu.Unlock()
		if err != nil {
			stderr.Write([]byte("kvz-init: pty: " + err.Error() + "\n"))
			sendExit(conn, &mu, 126)
			return
		}
		stdinW = ptmx
		copies.Add(1)
		go func() { defer copies.Done(); io.Copy(stdout, ptmx) }()
	} else {
		ro, wo, _ := os.Pipe()
		re, we, _ := os.Pipe()
		cmd.Stdout, cmd.Stderr = wo, we
		if req.Stdin {
			ri, wi, _ := os.Pipe()
			cmd.Stdin = ri
			stdinW = wi
			defer ri.Close()
		}
		pmu.Lock()
		err := cmd.Start()
		if err == nil {
			ch = registerWaiter(cmd.Process.Pid)
		}
		pmu.Unlock()
		wo.Close()
		we.Close()
		if cmd.Stdin != nil {
			cmd.Stdin.(*os.File).Close()
		}
		copies.Add(2)
		go func() { defer copies.Done(); io.Copy(stdout, ro); ro.Close() }()
		go func() { defer copies.Done(); io.Copy(stderr, re); re.Close() }()
		if err != nil {
			stderr.Write([]byte("kvz-init: " + err.Error() + "\n"))
			sendExit(conn, &mu, 127)
			return
		}
	}
	// Host -> guest frames.
	go func() {
		var h [5]byte
		for {
			if _, err := io.ReadFull(br, h[:]); err != nil {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			}
			p := make([]byte, binary.BigEndian.Uint32(h[1:]))
			if _, err := io.ReadFull(br, p); err != nil {
				return
			}
			switch h[0] {
			case fStdin:
				if stdinW != nil {
					stdinW.Write(p)
				}
			case fEOF:
				if stdinW != nil && !req.TTY {
					stdinW.Close()
				}
			case fResize:
				if ptmx != nil && len(p) == 4 {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: binary.BigEndian.Uint16(p[0:]), Cols: binary.BigEndian.Uint16(p[2:])})
				}
			}
		}
	}()
	code := <-ch
	copies.Wait() // pty: io.Copy ends with EIO once the child is gone
	if ptmx != nil {
		ptmx.Close()
	}
	sendExit(conn, &mu, code)
}

// registerWaiter must be called with pmu held.
func registerWaiter(pid int) chan int {
	c := make(chan int, 1)
	waiters[pid] = c
	return c
}

func sendExit(w io.Writer, mu *sync.Mutex, code int) {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(int32(code)))
	frameWriter{w, mu, fExit}.Write(p[:])
}
