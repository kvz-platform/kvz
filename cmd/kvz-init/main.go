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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	Env        []string `json:"env"`
	Cwd        string   `json:"cwd"`
	UID        int      `json:"uid"`
	GID        int      `json:"gid"`
	TTY        bool     `json:"tty"`
	ImageTag   string   `json:"image_tag"`         // virtiofs tag of the image rootfs
	Restart    string   `json:"restart"`           // Always | OnFailure | Never
	Sidecar    bool     `json:"sidecar,omitempty"` // restartable init container
	Groups     []int    `json:"groups,omitempty"`  // supplementary groups
	ReadOnly   bool     `json:"ro_root,omitempty"` // readOnlyRootFilesystem
	TermPath   string   `json:"term_path,omitempty"`
	TermLogs   bool     `json:"term_fallback_logs,omitempty"` // FallbackToLogsOnError
	Privileged bool     `json:"privileged,omitempty"`
	CapAdd     []string `json:"cap_add,omitempty"`
	CapDrop    []string `json:"cap_drop,omitempty"`
	NoNewPrivs bool     `json:"no_new_privs,omitempty"`

	Readiness *Probe   `json:"readiness,omitempty"`
	Liveness  *Probe   `json:"liveness,omitempty"`
	Startup   *Probe   `json:"startup,omitempty"`
	PostStart *Handler `json:"post_start,omitempty"`
	PreStop   *Handler `json:"pre_stop,omitempty"`
}

type Spec struct {
	Search     []string          `json:"search"`
	DNSOptions []string          `json:"dns_options,omitempty"`
	MTU        int               `json:"mtu"`
	IP         string            `json:"ip"`
	Mask       int               `json:"mask"`
	GW         string            `json:"gw"`
	Hostname   string            `json:"hostname"`
	DNS        []string          `json:"dns"`
	Hosts      string            `json:"hosts"`
	Containers []Container       `json:"containers"`
	Inits      []Container       `json:"init_containers,omitempty"`
	Restart    string            `json:"restart"` // pod restartPolicy
	Mounts     []Mount           `json:"mounts"`
	Sysctls    map[string]string `json:"sysctls,omitempty"`
}

type Mount struct {
	Container string `json:"container"`
	Tag       string `json:"tag"` // virtiofs tag (host dir) or "" for emptyDir path inside kvz share
	Sub       string `json:"sub"` // path inside the kvz share when Tag == ""
	Path      string `json:"path"`
	ReadOnly  bool   `json:"ro"`
	Tmpfs     string `json:"tmpfs,omitempty"`
	TmpfsSize int64  `json:"tmpfs_size,omitempty"`
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
	if len(os.Args) > 0 && os.Args[0] == "kvz-exec" {
		trampoline()
		return
	}
	t0 := bootMs()
	// Essential mounts only.
	must(mount("proc", "/proc", "proc", 0, ""), "mount /proc")
	must(mount("devtmpfs", "/dev", "devtmpfs", 0, ""), "mount /dev")
	must(mount("devpts", "/dev/pts", "devpts", 0, "ptmxmode=0666"), "mount /dev/pts")
	// what udev/runc normally provide: images symlink logs to /dev/stdout
	for l, t := range map[string]string{"/dev/fd": "/proc/self/fd", "/dev/stdin": "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1", "/dev/stderr": "/proc/self/fd/2", "/dev/core": "/proc/kcore"} {
		_ = os.Symlink(t, l)
	}
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
	podIP = spec.IP
	netDone := make(chan error, 1)
	go func() { netDone <- setupNetwork(spec.IP, spec.Mask, spec.GW, spec.MTU) }()

	// Per image: one read-only virtiofs lower; per container: overlay root.
	roots := map[string]string{}
	must(mount("tmpfs", "/rw", "tmpfs", 0, "mode=755"), "mount /rw")
	for _, c := range append(append([]Container{}, spec.Inits...), spec.Containers...) {
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
		_ = mount("shm", secureJoin(root, "/dev/shm"), "tmpfs", 0, "mode=1777")
		_ = mount("sysfs", secureJoin(root, "/sys"), "sysfs", unix.MS_RDONLY, "")
		_ = mount("tmpfs", secureJoin(root, "/run"), "tmpfs", 0, "mode=755")
		_ = mount("tmpfs", secureJoin(root, "/tmp"), "tmpfs", 0, "mode=1777")
		etc := secureJoin(root, "/etc")
		_ = os.MkdirAll(etc, 0o755)
		if len(spec.DNS) > 0 {
			var b strings.Builder
			for _, d := range spec.DNS {
				b.WriteString("nameserver " + d + "\n")
			}
			if len(spec.Search) > 0 {
				b.WriteString("search " + strings.Join(spec.Search, " ") + "\n")
			}
			if len(spec.DNSOptions) > 0 {
				b.WriteString("options " + strings.Join(spec.DNSOptions, " ") + "\n")
			}
			_ = os.WriteFile(etc+"/resolv.conf", []byte(b.String()), 0o644)
		}
		if spec.Hosts != "" {
			_ = os.WriteFile(etc+"/hosts", []byte(spec.Hosts), 0o644)
		}
		_ = os.WriteFile(etc+"/hostname", []byte(spec.Hostname+"\n"), 0o644)
		if c.TermPath != "" {
			tf := "/rw/" + c.Name + "/termination-log"
			_ = os.WriteFile(tf, nil, 0o666)
			_ = os.Chmod(tf, 0o666) // writable by any container user (umask)
			dst := secureJoin(root, c.TermPath)
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			if f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0o666); err == nil {
				f.Close()
			}
			_ = unix.Mount(tf, dst, "", unix.MS_BIND, "")
		}
		for _, m := range spec.Mounts {
			if m.Container != c.Name {
				continue
			}
			dst := secureJoin(root, m.Path)
			var flags uintptr
			if m.ReadOnly {
				flags |= unix.MS_RDONLY
			}
			switch {
			case m.Tmpfs != "":
				base := "/tmpfs/" + m.Tmpfs
				if _, err := os.Stat(base); err != nil {
					_ = os.MkdirAll(base, 0o777)
					opts := "mode=0777"
					if m.TmpfsSize > 0 {
						opts += fmt.Sprintf(",size=%d", m.TmpfsSize)
					}
					must(mount("tmpfs", base, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, opts), "tmpfs "+m.Tmpfs)
				}
				src := secureJoin(base, m.Sub)
				_ = os.MkdirAll(src, 0o777)
				must(mount(src, dst, "", unix.MS_BIND, ""), "volume "+m.Path)
			case m.Tag != "":
				must(mount("/v/"+m.Tag, dst, "", unix.MS_BIND, ""), "volume "+m.Path)
			default:
				src := filepath.Join("/kvz", m.Sub)
				_ = os.MkdirAll(src, 0o777)
				must(mount(src, dst, "", unix.MS_BIND, ""), "volume "+m.Path)
			}
			if flags&unix.MS_RDONLY != 0 { // a bind mount only becomes read-only on remount
				_ = unix.Mount("", dst, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, "")
			}
		}
	}

	for _, c := range append(append([]Container{}, spec.Inits...), spec.Containers...) {
		if c.ReadOnly {
			_ = unix.Mount("", "/c/"+c.Name, "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
		}
	}
	must(<-netDone, "network")
	for k, v := range spec.Sysctls {
		p := "/proc/sys/" + strings.ReplaceAll(k, ".", "/")
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			emit(Event{Type: "error", Msg: fmt.Sprintf("sysctl %s=%s: %v", k, v, err)})
		}
	}
	conn := <-connc
	if conn != nil {
		evOut = conn
	}
	emit(Event{Type: "hello", BootMs: t0, ReadyMs: bootMs()})

	// Init containers (sequential; sidecars keep running), then app containers.
	go func() {
		if !runInitContainers(spec.Inits, spec.Restart) {
			return
		}
		for _, c := range spec.Containers {
			startContainer(c)
		}
	}()

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
			stopMu.Lock()
			if cancel, ok := probeStop[name]; ok {
				cancel()
				delete(probeStop, name)
			}
			stopMu.Unlock()
			emit(Event{Type: "exit", Container: name, Code: code, Msg: terminationMessage(name, code)})
			exitMu.Lock()
			ch, isInit := exitChans[name]
			exitMu.Unlock()
			if isInit {
				ch <- code // the init sequence decides about retries
			} else {
				maybeRestart(name, code)
			}
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
	Grace     int    `json:"grace"`
}

func commands(conn *os.File) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var c Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch c.Type {
		case "kill":
			stopping.Store(true)
			if c.Container != "" {
				signalWhere(func(n string) bool { return n == c.Container }, syscall.Signal(c.Signal))
				continue
			}
			// Kubernetes order: preStop hooks, then main containers, and only
			// once they have exited the sidecars (restartable init containers).
			grace := time.Duration(max(c.Grace, 1)) * time.Second
			deadline := time.Now().Add(grace)
			preStopAll(grace)
			isSidecar := func(n string) bool { return specByName[n].Sidecar }
			signalWhere(func(n string) bool { return !isSidecar(n) }, syscall.Signal(c.Signal))
			for time.Now().Before(deadline) && countWhere(func(n string) bool { return !isSidecar(n) }) > 0 {
				time.Sleep(10 * time.Millisecond)
			}
			signalWhere(isSidecar, syscall.Signal(c.Signal))
		case "poweroff":
			unix.Sync()
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		}
	}
	_ = strconv.Itoa
}

func signalWhere(match func(string) bool, sig syscall.Signal) {
	pmu.Lock()
	defer pmu.Unlock()
	for pid, name := range pids {
		if match(name) {
			_ = unix.Kill(-pid, sig)
			_ = unix.Kill(pid, sig)
		}
	}
}

func countWhere(match func(string) bool) int {
	pmu.Lock()
	defer pmu.Unlock()
	n := 0
	for _, name := range pids {
		if match(name) {
			n++
		}
	}
	return n
}

var (
	stopping atomic.Bool
	backoff  = map[string]time.Duration{}
	boMu     sync.Mutex
)

func startContainer(c Container) { startContainerOpts(c, false) }

func startContainerOpts(c Container, restart bool) {
	logf := openLog(c.Name, restart)
	or, ow, _ := os.Pipe()
	er, ew, _ := os.Pipe()
	cmd, err := containerCmd(c, c.Argv)
	pmu.Lock()
	if err == nil {
		cmd.Stdout, cmd.Stderr = ow, ew
		cmd.SysProcAttr.Setsid = true
		err = cmd.Start()
	}
	if err == nil {
		pids[cmd.Process.Pid] = c.Name
	}
	pmu.Unlock()
	ow.Close()
	ew.Close()
	if err != nil {
		or.Close()
		er.Close()
		fmt.Fprintf(logf, "%s stderr F kvz-init: start %s: %v\n", time.Now().UTC().Format(time.RFC3339Nano), c.Name, err)
		logf.Close()
		emit(Event{Type: "exit", Container: c.Name, Code: 127, Msg: err.Error()})
		exitMu.Lock()
		ch, isInit := exitChans[c.Name]
		exitMu.Unlock()
		if isInit {
			ch <- 127
		} else {
			go maybeRestart(c.Name, 127)
		}
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pipeLines(or, logf, "stdout", &wg)
	go pipeLines(er, logf, "stderr", &wg)
	go func() { wg.Wait(); logf.Close() }()
	pid := cmd.Process.Pid
	emit(Event{Type: "started", Container: c.Name, Pid: pid, ReadyMs: bootMs()})
	ctx, cancel := context.WithCancel(context.Background())
	stopMu.Lock()
	probeStop[c.Name] = cancel
	stopMu.Unlock()
	if c.PostStart != nil {
		go func() {
			if err := runHandler(c, c.PostStart, 30*time.Second); err != nil {
				emit(Event{Type: "probe", Container: c.Name, Msg: "postStart hook failed: " + err.Error()})
				killInstance(pid)
				return
			}
			runProbes(ctx, c, pid)
		}()
	} else if c.Readiness != nil || c.Liveness != nil || c.Startup != nil || c.Sidecar {
		go runProbes(ctx, c, pid)
	}
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
	switch {
	case c.Sidecar:
		// sidecars always restart
	case c.Restart == "Never":
		return
	case c.Restart == "OnFailure":
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
			startContainerOpts(c, true) // rotates the log for `kubectl logs --previous`
		}
	}()
}

// secureJoin resolves path inside root, interpreting symlinks (absolute or
// relative) relative to root, so mounts never escape the container (e.g. the
// common /var/run -> /run symlink). Missing components are created as dirs.
func secureJoin(root, path string) string {
	cur := ""
	parts := strings.Split(filepath.Clean("/"+path), "/")
	for hops := 0; len(parts) > 0 && hops < 64; {
		p := parts[0]
		parts = parts[1:]
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			cur = filepath.Dir(cur)
			if cur == "." {
				cur = ""
			}
			continue
		}
		next := cur + "/" + p
		fi, err := os.Lstat(root + next)
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			hops++
			target, err := os.Readlink(root + next)
			if err != nil {
				cur = next
				continue
			}
			if filepath.IsAbs(target) {
				cur = ""
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		cur = next
	}
	return root + cur
}

// groups is the container's supplementary group list: the groups the image's
// /etc/group gives the user (like containerd), plus supplementalGroups/fsGroup.
func groups(c Container, root string) []uint32 {
	seen := map[uint32]bool{}
	var out []uint32
	add := func(g uint32) {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	if user := userName(root, c.UID); user != "" {
		if b, err := os.ReadFile(secureJoin(root, "/etc/group")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Split(line, ":")
				if len(f) < 4 {
					continue
				}
				for _, m := range strings.Split(f[3], ",") {
					if m == user {
						if gid, err := strconv.Atoi(f[2]); err == nil {
							add(uint32(gid))
						}
					}
				}
			}
		}
	}
	for _, g := range c.Groups {
		add(uint32(g))
	}
	return out
}

func userName(root string, uid int) string {
	b, err := os.ReadFile(secureJoin(root, "/etc/passwd"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 3 && f[2] == strconv.Itoa(uid) {
			return f[0]
		}
	}
	return ""
}

// terminationMessage implements terminationMessagePath/Policy: the file's
// content (max 4 KiB), or with FallbackToLogsOnError the log tail on failure.
func terminationMessage(name string, code int) string {
	c, ok := specByName[name]
	if !ok || c.TermPath == "" {
		return ""
	}
	b, _ := os.ReadFile("/rw/" + name + "/termination-log")
	if len(b) > 4096 {
		b = b[:4096]
	}
	if len(b) == 0 && c.TermLogs && code != 0 {
		raw, _ := os.ReadFile(logPath(name))
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) > 80 {
			lines = lines[len(lines)-80:]
		}
		var out strings.Builder
		for _, l := range lines {
			// "<ts> <stream> <tag> <msg>": F lines end in a newline, P (partial) don't
			if f := strings.SplitN(l, " ", 4); len(f) == 4 {
				out.WriteString(f[3])
				if f[2] == "F" {
					out.WriteString("\n")
				}
				continue
			}
			out.WriteString(l + "\n")
		}
		msg := out.String()
		if len(msg) > 2048 {
			msg = msg[len(msg)-2048:]
		}
		return msg
	}
	return string(b)
}
