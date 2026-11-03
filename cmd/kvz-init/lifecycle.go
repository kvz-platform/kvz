//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Probe mirrors corev1.Probe with named ports already resolved by the host.
type Probe struct {
	Exec        []string          `json:"exec,omitempty"`
	HTTPPath    string            `json:"http_path,omitempty"`
	HTTPPort    int               `json:"http_port,omitempty"`
	HTTPScheme  string            `json:"http_scheme,omitempty"`
	HTTPHost    string            `json:"http_host,omitempty"`
	HTTPHeaders map[string]string `json:"http_headers,omitempty"`
	TCPPort     int               `json:"tcp_port,omitempty"`
	Initial     int               `json:"initial,omitempty"`
	Period      int               `json:"period,omitempty"`
	Timeout     int               `json:"timeout,omitempty"`
	Success     int               `json:"success,omitempty"`
	Failure     int               `json:"failure,omitempty"`
}

// Handler mirrors corev1.LifecycleHandler.
type Handler struct {
	Exec     []string `json:"exec,omitempty"`
	HTTPPath string   `json:"http_path,omitempty"`
	HTTPPort int      `json:"http_port,omitempty"`
	HTTPHost string   `json:"http_host,omitempty"`
	TCPPort  int      `json:"tcp_port,omitempty"`
	Sleep    int      `json:"sleep,omitempty"`
}

var insecureTransport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // like the kubelet's HTTPS probes

var (
	podIP     string
	stopMu    sync.Mutex
	probeStop = map[string]context.CancelFunc{} // container -> cancel its probe loops
	exitChans = map[string]chan int{}           // container -> exit codes (init containers)
	exitMu    sync.Mutex
	logMu     sync.Mutex
)

// ---- logging: CRI format "<RFC3339Nano> <stream> F <line>" -----------------

func logPath(name string) string { return "/kvz/logs/" + name + ".log" }

// openLog rotates the previous instance's log to .log.1 (kubectl logs --previous).
func openLog(name string, restart bool) *os.File {
	_ = os.MkdirAll("/kvz/logs", 0o755)
	if restart {
		_ = os.Rename(logPath(name), logPath(name)+".1")
	}
	f, _ := os.OpenFile(logPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	return f
}

// pipeLines copies r into out, one CRI-formatted line per line read.
func pipeLines(r io.ReadCloser, out *os.File, stream string, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			tag := "F"
			if line[len(line)-1] != '\n' {
				tag, line = "P", line+"\n"
			}
			logMu.Lock()
			out.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + " " + stream + " " + tag + " " + line)
			logMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// ---- running commands inside a container root ------------------------------

// runIn executes argv in the container's root with its env and identity and
// returns the exit code (the reaper delivers it through waiters).
func runIn(c Container, argv []string, timeout time.Duration) (int, string) {
	if len(argv) == 0 {
		return 1, "empty command"
	}
	cmd, err := containerCmd(c, argv)
	if err != nil {
		return 126, err.Error()
	}
	r, w, _ := os.Pipe()
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr.Setsid = true
	pmu.Lock()
	err = cmd.Start()
	var ch chan int
	if err == nil {
		ch = registerWaiter(cmd.Process.Pid)
	}
	pmu.Unlock()
	w.Close()
	if err != nil {
		r.Close()
		return 127, err.Error()
	}
	outc := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 4096))
		r.Close()
		outc <- string(b)
	}()
	select {
	case code := <-ch:
		return code, <-outc
	case <-time.After(timeout):
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		<-ch
		return 1, "timeout"
	}
}

// ---- probes ------------------------------------------------------------------

func secs(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

func probeOnce(c Container, p *Probe) bool {
	timeout := secs(p.Timeout, 1)
	switch {
	case len(p.Exec) > 0:
		code, _ := runIn(c, p.Exec, timeout)
		return code == 0
	case p.HTTPPort > 0:
		host := p.HTTPHost
		if host == "" {
			host = podIP
		}
		scheme := "http"
		if p.HTTPScheme == "HTTPS" {
			scheme = "https"
		}
		req, err := http.NewRequest("GET", fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, strconv.Itoa(p.HTTPPort)), p.HTTPPath), nil)
		if err != nil {
			return false
		}
		for k, v := range p.HTTPHeaders {
			req.Header.Set(k, v)
		}
		req.Header.Set("User-Agent", "kube-probe/1.37")
		cl := &http.Client{Timeout: timeout, Transport: insecureTransport}
		resp, err := cl.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	case p.TCPPort > 0:
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(podIP, strconv.Itoa(p.TCPPort)), timeout)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	return true
}

// runProbes drives startup -> (readiness, liveness) for one container instance.
// onStarted is called once the startup probe passes (or immediately).
func runProbes(ctx context.Context, c Container, pid int) {
	sleepCtx := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	if p := c.Startup; p != nil {
		if !sleepCtx(secs(p.Initial, 0)) {
			return
		}
		fails := 0
		for {
			if probeOnce(c, p) {
				break
			}
			fails++
			if fails >= max(p.Failure, 1) {
				emit(Event{Type: "probe", Container: c.Name, Msg: "startup probe failed"})
				killInstance(pid)
				return
			}
			if !sleepCtx(secs(p.Period, 10)) {
				return
			}
		}
	}
	emit(Event{Type: "startup-ok", Container: c.Name})
	if c.Readiness == nil {
		emit(Event{Type: "ready", Container: c.Name})
	} else {
		go func() {
			p := c.Readiness
			if !sleepCtx(secs(p.Initial, 0)) {
				return
			}
			ready, ok, fail := false, 0, 0
			for {
				if probeOnce(c, p) {
					ok, fail = ok+1, 0
					if !ready && ok >= max(p.Success, 1) {
						ready = true
						emit(Event{Type: "ready", Container: c.Name})
					}
				} else {
					fail, ok = fail+1, 0
					if ready && fail >= max(p.Failure, 1) {
						ready = false
						emit(Event{Type: "unready", Container: c.Name})
					}
				}
				if !sleepCtx(secs(p.Period, 10)) {
					return
				}
			}
		}()
	}
	if p := c.Liveness; p != nil {
		go func() {
			if !sleepCtx(secs(p.Initial, 0)) {
				return
			}
			fail := 0
			for {
				if probeOnce(c, p) {
					fail = 0
				} else {
					fail++
					if fail >= max(p.Failure, 1) {
						emit(Event{Type: "probe", Container: c.Name, Msg: "liveness probe failed"})
						killInstance(pid)
						return
					}
				}
				if !sleepCtx(secs(p.Period, 10)) {
					return
				}
			}
		}()
	}
}

func killInstance(pid int) {
	_ = unix.Kill(-pid, unix.SIGTERM)
	go func() {
		time.Sleep(2 * time.Second)
		_ = unix.Kill(-pid, unix.SIGKILL)
		_ = unix.Kill(pid, unix.SIGKILL)
	}()
}

// ---- lifecycle hooks -----------------------------------------------------------

func runHandler(c Container, h *Handler, timeout time.Duration) error {
	switch {
	case h.Sleep > 0:
		time.Sleep(time.Duration(h.Sleep) * time.Second)
	case len(h.Exec) > 0:
		if code, out := runIn(c, h.Exec, timeout); code != 0 {
			return fmt.Errorf("exit %d: %s", code, out)
		}
	case h.HTTPPort > 0:
		host := h.HTTPHost
		if host == "" {
			host = podIP
		}
		cl := &http.Client{Timeout: timeout}
		resp, err := cl.Get(fmt.Sprintf("http://%s%s", net.JoinHostPort(host, strconv.Itoa(h.HTTPPort)), h.HTTPPath))
		if err != nil {
			return err
		}
		resp.Body.Close()
	case h.TCPPort > 0:
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(podIP, strconv.Itoa(h.TCPPort)), timeout)
		if err != nil {
			return err
		}
		conn.Close()
	}
	return nil
}

// preStopAll runs every running container's preStop hook in parallel.
func preStopAll(grace time.Duration) {
	var wg sync.WaitGroup
	pmu.Lock()
	running := map[string]bool{}
	for _, n := range pids {
		running[n] = true
	}
	pmu.Unlock()
	for name := range running {
		c, ok := specByName[name]
		if !ok || c.PreStop == nil {
			continue
		}
		wg.Add(1)
		go func(c Container) {
			defer wg.Done()
			_ = runHandler(c, c.PreStop, grace)
		}(c)
	}
	wg.Wait()
}

// ---- init containers --------------------------------------------------------------

func exitChan(name string) chan int {
	exitMu.Lock()
	defer exitMu.Unlock()
	ch, ok := exitChans[name]
	if !ok {
		ch = make(chan int, 4)
		exitChans[name] = ch
	}
	return ch
}

// runInitContainers runs init containers in order (sidecars are started and
// left running). It returns false if the pod must fail (restartPolicy Never).
func runInitContainers(inits []Container, podRestart string) bool {
	for _, c := range inits {
		if c.Sidecar {
			startContainer(c)
			continue
		}
		backoff := time.Second
		for attempt := 0; ; attempt++ {
			if stopping.Load() {
				return false
			}
			ch := exitChan(c.Name)
			startContainerOpts(c, attempt > 0)
			code := <-ch
			if code == 0 {
				break
			}
			if podRestart == "Never" {
				emit(Event{Type: "init-failed", Container: c.Name, Code: code})
				return false
			}
			time.Sleep(backoff)
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}
	return true
}
