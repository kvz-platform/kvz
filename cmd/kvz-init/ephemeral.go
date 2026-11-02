//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// bootSpec is the pod spec kvz-init booted with (DNS, hosts, hostname).
var bootSpec Spec

var (
	specMu    sync.Mutex
	ttys      = map[string]*os.File{}               // pty masters of interactive containers
	attachers = map[string]map[io.Writer]struct{}{} // clients attached to them
	ttyDone   = map[string]chan struct{}{}          // closed when a console ends
)

// attachTTY connects an exec-protocol client to a container's console:
// stdin and resize frames go to the pty, output is fanned out to it.
func attachTTY(name string, br *bufio.Reader, stdout io.Writer) bool {
	specMu.Lock()
	ptmx := ttys[name]
	if ptmx != nil {
		if attachers[name] == nil {
			attachers[name] = map[io.Writer]struct{}{}
		}
		attachers[name][stdout] = struct{}{}
	}
	specMu.Unlock()
	if ptmx == nil {
		return false
	}
	specMu.Lock()
	done := ttyDone[name]
	specMu.Unlock()
	defer func() {
		specMu.Lock()
		delete(attachers[name], stdout)
		specMu.Unlock()
	}()
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		var h [5]byte
		for {
			if _, err := io.ReadFull(br, h[:]); err != nil {
				return
			}
			p := make([]byte, binary.BigEndian.Uint32(h[1:]))
			if _, err := io.ReadFull(br, p); err != nil {
				return
			}
			switch h[0] {
			case fStdin:
				_, _ = ptmx.Write(p)
			case fResize:
				if len(p) == 4 {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: binary.BigEndian.Uint16(p[0:]), Cols: binary.BigEndian.Uint16(p[2:])})
				}
			}
		}
	}()
	select { // the session ends when the container exits or the client leaves
	case <-done:
	case <-clientGone:
	}
	return true
}

// addContainer starts a container in the running pod (ephemeral containers).
func addContainer(c Container, mounts []Mount) {
	specMu.Lock()
	if _, exists := specByName[c.Name]; exists {
		specMu.Unlock()
		return
	}
	specByName[c.Name] = c
	specMu.Unlock()
	root := "/c/" + c.Name
	up, wk := "/rw/"+c.Name+"/u", "/rw/"+c.Name+"/w"
	_ = os.MkdirAll(up, 0o755)
	_ = os.MkdirAll(wk, 0o755)
	if err := mount("overlay", root, "overlay", 0, "lowerdir=/v/"+c.ImageTag+",upperdir="+up+",workdir="+wk); err != nil {
		emit(Event{Type: "exit", Container: c.Name, Code: 128, Msg: "overlay: " + err.Error()})
		return
	}
	_ = mount("proc", root+"/proc", "proc", 0, "")
	_ = mount("/dev", root+"/dev", "", unix.MS_BIND|unix.MS_REC, "")
	_ = mount("shm", secureJoin(root, "/dev/shm"), "tmpfs", 0, "mode=1777")
	_ = mount("sysfs", secureJoin(root, "/sys"), "sysfs", unix.MS_RDONLY, "")
	_ = mount("tmpfs", secureJoin(root, "/run"), "tmpfs", 0, "mode=755")
	_ = mount("tmpfs", secureJoin(root, "/tmp"), "tmpfs", 0, "mode=1777")
	etc := secureJoin(root, "/etc")
	_ = os.MkdirAll(etc, 0o755)
	// same resolver, hosts and hostname as the pod's other containers
	for _, f := range []string{"resolv.conf", "hosts", "hostname"} {
		for name := range specByName {
			if name == c.Name {
				continue
			}
			if b, err := os.ReadFile(secureJoin("/c/"+name, "/etc/"+f)); err == nil {
				_ = os.WriteFile(filepath.Join(etc, f), b, 0o644)
				break
			}
		}
	}
	for _, m := range mounts {
		dst := secureJoin(root, m.Path)
		src := filepath.Join("/kvz", m.Sub)
		if m.Tmpfs != "" {
			src = secureJoin("/tmpfs/"+m.Tmpfs, m.Sub)
		}
		if err := mount(src, dst, "", unix.MS_BIND, ""); err == nil && m.ReadOnly {
			_ = unix.Mount("", dst, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, "")
		}
	}
	if c.TTY {
		startTTYContainer(c)
		return
	}
	startContainer(c)
}

// startTTYContainer runs an interactive container on a pty that stays open
// (like a container runtime holding the console), so shells keep running.
func startTTYContainer(c Container) {
	cmd, err := containerCmd(c, c.Argv)
	if err != nil {
		emit(Event{Type: "exit", Container: c.Name, Code: 127, Msg: err.Error()})
		return
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	pmu.Lock()
	ptmx, err := pty.Start(cmd)
	if err == nil {
		pids[cmd.Process.Pid] = c.Name
	}
	pmu.Unlock()
	if err != nil {
		emit(Event{Type: "exit", Container: c.Name, Code: 128, Msg: err.Error()})
		return
	}
	done := make(chan struct{})
	specMu.Lock()
	ttys[c.Name] = ptmx
	ttyDone[c.Name] = done
	specMu.Unlock()
	logf := openLog(c.Name, false)
	pr, pw := io.Pipe()
	go func() { // console output: to attached clients and to the log
		buf := make([]byte, 32<<10)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				specMu.Lock()
				for w := range attachers[c.Name] {
					_, _ = w.Write(buf[:n])
				}
				specMu.Unlock()
				_, _ = pw.Write(buf[:n])
			}
			if err != nil { // EIO once the process (and the last pty user) is gone
				pw.Close()
				close(done)
				return
			}
		}
	}()
	go func() {
		defer logf.Close()
		br := bufio.NewReader(pr)
		for {
			line, err := br.ReadString('\n')
			if len(line) > 0 {
				tag := "F"
				if !strings.HasSuffix(line, "\n") {
					tag, line = "P", line+"\n"
				}
				line = strings.TrimSuffix(line, "\r\n") + "\n"
				logMu.Lock()
				logf.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + " stdout " + tag + " " + line)
				logMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	emit(Event{Type: "started", Container: c.Name, Pid: cmd.Process.Pid, ReadyMs: bootMs()})
}
