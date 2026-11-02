//go:build linux

// kvz-init is PID 1 inside a kvz Linux microVM. It is deliberately tiny and
// does the minimum on the critical path:
//
//  1. mount /proc, /dev, the "kvz" virtiofs share (pod spec + logs) and the
//     "img" virtiofs share (image rootfs, read-only, shared by every pod VM
//     using that image)
//  2. build an overlay root: lower = image, upper = tmpfs (per pod, in guest RAM)
//  3. fork/chroot/exec every container entrypoint from spec.json
//  4. push "started"/"exit" events to the host over vsock
//
// Networking is configured by the kernel from the `ip=` cmdline (no DHCP), so
// the pod IP is known to the host before the VM boots.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Container struct {
	Name     string   `json:"name"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
	UID      int      `json:"uid"`
	GID      int      `json:"gid"`
	TTY      bool     `json:"tty"`
	ImageTag string   `json:"image_tag"` // virtiofs tag of the image rootfs
	Restart  string   `json:"restart"`   // Always | OnFailure | Never
}

type Spec struct {
	IP         string      `json:"ip"`
	Mask       int         `json:"mask"`
	GW         string      `json:"gw"`
	Hostname   string      `json:"hostname"`
	DNS        []string    `json:"dns"`
	Hosts      string      `json:"hosts"`
	Containers []Container `json:"containers"`
	Mounts     []Mount     `json:"mounts"`
}

type Mount struct {
	Container string `json:"container"`
	Tag       string `json:"tag"` // virtiofs tag (host dir) or "" for emptyDir path inside kvz share
	Sub       string `json:"sub"` // path inside the kvz share when Tag == ""
	Path      string `json:"path"`
	ReadOnly  bool   `json:"ro"`
}

type Event struct {
	Type      string  `json:"type"`
	Container string  `json:"container,omitempty"`
	Pid       int     `json:"pid,omitempty"`
	Code      int     `json:"code,omitempty"`
	Msg       string  `json:"msg,omitempty"`
	BootMs    float64 `json:"boot_ms,omitempty"` // CLOCK_BOOTTIME when init started
	ReadyMs   float64 `json:"ready_ms,omitempty"`
}

var (
	evMu  sync.Mutex
	evOut io.Writer = io.Discard

	// pmu guards pids/waiters; hold it across fork+register so the reaper
	// can never collect a child before it is registered.
	pmu     sync.Mutex
	pids    = map[int]string{}
	waiters = map[int]chan int{}
)

func emit(e Event) {
	b, _ := json.Marshal(e)
	evMu.Lock()
	evOut.Write(append(b, '\n'))
	evMu.Unlock()
}

func bootMs() float64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return float64(ts.Sec)*1e3 + float64(ts.Nsec)/1e6
}

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "kvz-init: %s: %v\n", what, err)
		emit(Event{Type: "error", Msg: what + ": " + err.Error()})
	}
}

func mount(src, dst, fstype string, flags uintptr, data string) error {
	_ = os.MkdirAll(dst, 0o755)
	return unix.Mount(src, dst, fstype, flags, data)
}

func dialHost() (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: 1024}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock"), nil
}

func main() {
	t0 := bootMs()
	// Essential mounts only.
	must(mount("proc", "/proc", "proc", 0, ""), "mount /proc")
	must(mount("devtmpfs", "/dev", "devtmpfs", 0, ""), "mount /dev")
	must(mount("devpts", "/dev/pts", "devpts", 0, "ptmxmode=0666"), "mount /dev/pts")
	must(mount("kvz", "/v", "virtiofs", 0, ""), "mount kvz share")
	must(mount("/v/pod", "/kvz", "", unix.MS_BIND, ""), "bind pod dir")
	go serveExec()

	// Connect the event channel early (in parallel with rootfs setup) and
	// announce ourselves so the host can timestamp kernel boot.
	connc := make(chan *os.File, 1)
	go func() {
		for i := 0; i < 2000; i++ {
			if f, err := dialHost(); err == nil {
				b, _ := json.Marshal(Event{Type: "init", BootMs: t0, ReadyMs: bootMs()})
				f.Write(append(b, '\n'))
				connc <- f
				return
			}
			time.Sleep(time.Millisecond)
		}
		connc <- nil
	}()

	raw, err := os.ReadFile("/kvz/spec.json")
	must(err, "read spec")
	var spec Spec
	must(json.Unmarshal(raw, &spec), "parse spec")
	if spec.Hostname != "" {
		_ = unix.Sethostname([]byte(spec.Hostname))
	}
	netDone := make(chan error, 1)
	go func() { netDone <- setupNetwork(spec.IP, spec.Mask, spec.GW) }()

	// Per image: one read-only virtiofs lower; per container: overlay root.
	roots := map[string]string{}
	must(mount("tmpfs", "/rw", "tmpfs", 0, "mode=755"), "mount /rw")
	for _, c := range spec.Containers {
		specByName[c.Name] = c
		lower := "/v/" + c.ImageTag
		roots[lower] = lower
		root := "/c/" + c.Name
		up, wk := "/rw/"+c.Name+"/u", "/rw/"+c.Name+"/w"
		_ = os.MkdirAll(up, 0o755)
		_ = os.MkdirAll(wk, 0o755)
		must(mount("overlay", root, "overlay", 0, "lowerdir="+lower+",upperdir="+up+",workdir="+wk), "overlay "+c.Name)
		must(mount("proc", root+"/proc", "proc", 0, ""), "proc "+c.Name)
		must(mount("/dev", root+"/dev", "", unix.MS_BIND|unix.MS_REC, ""), "dev "+c.Name)
		_ = mount("shm", root+"/dev/shm", "tmpfs", 0, "mode=1777")
		_ = mount("sysfs", root+"/sys", "sysfs", unix.MS_RDONLY, "")
		_ = mount("tmpfs", root+"/run", "tmpfs", 0, "mode=755")
		_ = mount("tmpfs", root+"/tmp", "tmpfs", 0, "mode=1777")
		etc := root + "/etc"
		_ = os.MkdirAll(etc, 0o755)
		if len(spec.DNS) > 0 {
			var b strings.Builder
			for _, d := range spec.DNS {
				b.WriteString("nameserver " + d + "\n")
			}
			_ = os.WriteFile(etc+"/resolv.conf", []byte(b.String()), 0o644)
		}
		if spec.Hosts != "" {
			_ = os.WriteFile(etc+"/hosts", []byte(spec.Hosts), 0o644)
		}
		_ = os.WriteFile(etc+"/hostname", []byte(spec.Hostname+"\n"), 0o644)
		for _, m := range spec.Mounts {
			if m.Container != c.Name {
				continue
			}
			dst := root + m.Path
			var flags uintptr
			if m.ReadOnly {
				flags |= unix.MS_RDONLY
			}
			if m.Tag != "" {
				must(mount("/v/"+m.Tag, dst, "", unix.MS_BIND|flags, ""), "volume "+m.Path)
			} else {
				src := filepath.Join("/kvz", m.Sub)
				_ = os.MkdirAll(src, 0o777)
				must(mount(src, dst, "", unix.MS_BIND, ""), "volume "+m.Path)
			}
		}
	}

	must(<-netDone, "network")
	conn := <-connc
	if conn != nil {
		evOut = conn
	}
	emit(Event{Type: "hello", BootMs: t0, ReadyMs: bootMs()})

	// Start every container.
	for _, c := range spec.Containers {
		startContainer(c)
	}

	// Host commands (kill, exec) arrive on the same connection.
	if conn != nil {
		go commands(conn)
	}

	// Reap: PID 1 must wait for everything.
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.ECHILD {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			continue
		}
		code := ws.ExitStatus()
		if ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		pmu.Lock()
		name, ok := pids[pid]
		delete(pids, pid)
		w, wok := waiters[pid]
		delete(waiters, pid)
		pmu.Unlock()
		if wok {
			w <- code
		}
		if ok {
			emit(Event{Type: "exit", Container: name, Code: code})
			maybeRestart(name, code)
		}
	}
}

// resolve finds argv0 on PATH inside the container root.
func resolve(root, argv0 string, env []string) string {
	if strings.Contains(argv0, "/") {
		return argv0
	}
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			path = e[5:]
		}
	}
	for _, d := range strings.Split(path, ":") {
		p := filepath.Join(d, argv0)
		if st, err := os.Stat(root + p); err == nil && !st.IsDir() {
			return p
		}
		// symlinks inside the root (e.g. busybox applets)
		if _, err := os.Lstat(root + p); err == nil {
			return p
		}
	}
	return argv0
}

type Command struct {
	Type      string `json:"type"`
	Container string `json:"container"`
	Signal    int    `json:"signal"`
}

func commands(conn *os.File) {
	mu := &pmu
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var c Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch c.Type {
		case "kill":
			stopping.Store(true)
			mu.Lock()
			for pid, name := range pids {
				if c.Container == "" || c.Container == name {
					_ = unix.Kill(-pid, syscall.Signal(c.Signal))
					_ = unix.Kill(pid, syscall.Signal(c.Signal))
				}
			}
			mu.Unlock()
		case "poweroff":
			unix.Sync()
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		}
	}
	_ = strconv.Itoa
}

var (
	stopping atomic.Bool
	backoff  = map[string]time.Duration{}
	boMu     sync.Mutex
)

func startContainer(c Container) {
	logf, err := os.OpenFile("/kvz/logs/"+c.Name+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = os.MkdirAll("/kvz/logs", 0o755)
		logf, _ = os.OpenFile("/kvz/logs/"+c.Name+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	defer logf.Close() // the child keeps its own descriptors
	root := "/c/" + c.Name
	argv0 := resolve(root, c.Argv[0], c.Env)
	cmd := &exec.Cmd{Path: argv0, Args: c.Argv, Env: c.Env, Dir: c.Cwd, Stdout: logf, Stderr: logf,
		SysProcAttr: &syscall.SysProcAttr{Chroot: root, Setsid: true,
			Credential: &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID)}}}
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	pmu.Lock()
	if err := cmd.Start(); err != nil {
		pmu.Unlock()
		fmt.Fprintf(logf, "kvz-init: start %s: %v\n", c.Name, err)
		emit(Event{Type: "exit", Container: c.Name, Code: 127, Msg: err.Error()})
		go maybeRestart(c.Name, 127)
		return
	}
	pids[cmd.Process.Pid] = c.Name
	pmu.Unlock()
	emit(Event{Type: "started", Container: c.Name, Pid: cmd.Process.Pid, ReadyMs: bootMs()})
}

// maybeRestart implements the pod restartPolicy with kubelet-style
// exponential backoff (1s, 2s, 4s ... capped at 60s; reset after 60s up).
func maybeRestart(name string, code int) {
	if stopping.Load() {
		return
	}
	c, ok := specByName[name]
	if !ok {
		return
	}
	switch c.Restart {
	case "Never":
		return
	case "OnFailure":
		if code == 0 {
			return
		}
	}
	boMu.Lock()
	d := backoff[name]
	if d == 0 {
		d = time.Second
	} else if d < 60*time.Second {
		d *= 2
	}
	backoff[name] = d
	boMu.Unlock()
	go func() {
		time.Sleep(d)
		if !stopping.Load() {
			startContainer(c)
		}
	}()
}
