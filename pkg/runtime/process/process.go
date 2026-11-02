// Package process runs pod containers as native macOS processes. The image
// field is informational; command/args name a host executable. Each pod gets
// its own working directory and log files. Pod IP == host IP.
package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"

	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

type Backend struct {
	dir    string
	hostIP string
	t      *rt.Tracker
	mu     sync.Mutex
	procs  map[string]*podProcs
}

type podProcs struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	cmds   map[string]*exec.Cmd
	mu     sync.Mutex
}

func New(dataDir, hostIP string, t *rt.Tracker) *Backend {
	return &Backend{dir: filepath.Join(dataDir, "pods"), hostIP: hostIP, t: t, procs: map[string]*podProcs{}}
}

func (b *Backend) Handler() string { return "darwin" }

func (b *Backend) podDir(pod *corev1.Pod) string { return filepath.Join(b.dir, string(pod.UID)) }

func (b *Backend) Start(ctx context.Context, pod *corev1.Pod) error {
	dir := b.podDir(pod)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	pctx, cancel := context.WithCancel(context.Background())
	pp := &podProcs{cancel: cancel, cmds: map[string]*exec.Cmd{}}
	b.mu.Lock()
	b.procs[rt.Key(pod.Namespace, pod.Name)] = pp
	b.mu.Unlock()
	b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
		s.PodIP = b.hostIP
		s.PodIPs = []corev1.PodIP{{IP: b.hostIP}}
	})
	for _, c := range pod.Spec.Containers {
		c := c
		pp.wg.Add(1)
		go func() {
			defer pp.wg.Done()
			b.supervise(pctx, pod, c, dir, pp)
		}()
	}
	return nil
}

func (b *Backend) supervise(ctx context.Context, pod *corev1.Pod, c corev1.Container, dir string, pp *podProcs) {
	argv := append(append([]string{}, c.Command...), c.Args...)
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		if len(argv) == 0 {
			b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
				rt.SetWaiting(s, c.Name, "InvalidCommand", "darwin runtime requires command")
			})
			return
		}
		logf, err := os.OpenFile(filepath.Join(dir, c.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		if c.WorkingDir != "" {
			cmd.Dir = c.WorkingDir
		}
		cmd.Env = append(append(os.Environ(), "KVZ_POD_DIR="+dir, "HOSTNAME="+pod.Name), rt.Env(c)...)
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			logf.Close()
			b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
				rt.SetTerminated(s, c.Name, 127, "StartError")
			})
			return
		}
		pp.mu.Lock()
		pp.cmds[c.Name] = cmd
		pp.mu.Unlock()
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
			rt.SetRunning(s, c.Name, fmt.Sprintf("darwin://%d", cmd.Process.Pid), true)
		})
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var werr error
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			grace := 10 * time.Second
			if pod.Spec.TerminationGracePeriodSeconds != nil {
				grace = time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
			}
			select {
			case <-done:
			case <-time.After(grace):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
			logf.Close()
			return
		case werr = <-done:
		}
		logf.Close()
		code := int32(0)
		if werr != nil {
			code = 1
			if ee, ok := werr.(*exec.ExitError); ok {
				code = int32(ee.ExitCode())
			}
		}
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) { rt.SetTerminated(s, c.Name, code, reason) })
		rp := pod.Spec.RestartPolicy
		if rp == corev1.RestartPolicyNever || (rp == corev1.RestartPolicyOnFailure && code == 0) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) { rt.IncRestart(s, c.Name) })
	}
}

func (b *Backend) Stop(ctx context.Context, pod *corev1.Pod) error {
	k := rt.Key(pod.Namespace, pod.Name)
	b.mu.Lock()
	pp := b.procs[k]
	delete(b.procs, k)
	b.mu.Unlock()
	if pp != nil {
		pp.cancel()
		pp.wg.Wait()
	}
	return os.RemoveAll(b.podDir(pod))
}

func (b *Backend) Logs(ctx context.Context, pod *corev1.Pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(b.podDir(pod), container+".log"))
	if err != nil {
		return nil, err
	}
	if !opts.Follow {
		return f, nil
	}
	return rt.Follow(ctx, f), nil
}

func (b *Backend) Exec(ctx context.Context, pod *corev1.Pod, container string, cmdv []string, attach api.AttachIO) error {
	var env []string
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			env = rt.Env(c)
		}
	}
	cmd := exec.CommandContext(ctx, cmdv[0], cmdv[1:]...)
	cmd.Dir = b.podDir(pod)
	cmd.Env = append(append(os.Environ(), "KVZ_POD_DIR="+cmd.Dir), env...)
	if attach.TTY() {
		f, err := pty.Start(cmd)
		if err != nil {
			return err
		}
		defer f.Close()
		go func() {
			for sz := range attach.Resize() {
				_ = pty.Setsize(f, &pty.Winsize{Rows: sz.Height, Cols: sz.Width})
			}
		}()
		if attach.Stdin() != nil {
			go io.Copy(f, attach.Stdin())
		}
		if attach.Stdout() != nil {
			go io.Copy(attach.Stdout(), f)
		}
		return cmd.Wait()
	}
	if attach.Stdin() != nil {
		cmd.Stdin = attach.Stdin()
	}
	if attach.Stdout() != nil {
		cmd.Stdout = attach.Stdout()
	}
	if attach.Stderr() != nil {
		cmd.Stderr = attach.Stderr()
	}
	return cmd.Run()
}
