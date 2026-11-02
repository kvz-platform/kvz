//go:build darwin

// Package microvmrt is the kvz "linux" runtime: one kvz microVM per pod,
// booted cold for every pod (no pools, no snapshots). All containers of a pod
// share the VM, so they share a network namespace (one pod IP) like real
// Kubernetes pods. The pod IP is allocated by the host and handed to the
// kernel on its cmdline, so it is published before the VM even boots.
package microvmrt

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/image"
	"github.com/GrowlyX/kvz/pkg/microvm"
	"github.com/GrowlyX/kvz/pkg/podnet"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
	"github.com/GrowlyX/kvz/pkg/runtime/volumes"
)

//go:embed kvz-init
var initBin []byte

type Backend struct {
	hostIP        string
	dir           string
	kernel        string
	kernelHotplug bool // pod kernel can online CPUs at runtime
	kernelCgroups bool // pod kernel enforces per-container cgroup limits
	initrd        string
	t             *rt.Tracker
	vols          *volumes.Materializer
	images        *image.Store
	pnet          *podnet.Net
	dns           []string

	mu   sync.Mutex
	pods map[string]*pod
	pre  map[string]net.IP // IPs reserved by PrepareIP before Start

	// slots caps concurrently running pod VMs below Virtualization.framework's
	// per-host limit (~127, measured): beyond it VZ fails VM starts outright.
	slots chan struct{}
}

type pod struct {
	images    map[string]*image.Image // container image ref -> resolved image
	listeners []net.Listener          // hostPort / hostNetwork forwards
	stopping  atomic.Bool             // set by Stop: exits are final, never CrashLoopBackOff
	vm        *microvm.VM
	ip        net.IP
	dir       string
	cancel    context.CancelFunc
	done      chan struct{}
	names     []string
	latest    atomic.Pointer[corev1.Pod] // newest pod object from UpdatePod
	first     *corev1.Pod                // pod as started (resources applied at boot)
	resMu     sync.Mutex
	applied   map[string]corev1.ResourceRequirements // container resources in effect
	ns        string
	refresh   chan struct{} // re-render configMap/secret/downwardAPI volumes
}

func DefaultKernel() string {
	if k := os.Getenv("KVZ_KERNEL"); k != "" {
		return k
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, ".kvz", "kernel", "Image"); exists(p) {
		return p
	}
	// Packaged installs (Homebrew) ship the kernel at <prefix>/share/kvz/kernel/Image
	// next to <prefix>/bin/kvz.
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if p := filepath.Join(filepath.Dir(exe), "..", "share", "kvz", "kernel", "Image"); exists(p) {
			return filepath.Clean(p)
		}
	}
	return filepath.Join(home, "Library", "Application Support", "com.apple.container", "kernels", "default.kernel-arm64")
}

func consoleLog(dir string) string {
	if os.Getenv("KVZ_VM_CONSOLE") != "" {
		return filepath.Join(dir, "console.log")
	}
	return ""
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func New(dataDir, hostIP string, t *rt.Tracker, vols *volumes.Materializer, pn *podnet.Net) (*Backend, error) {
	home, _ := os.UserHomeDir()
	initrd, err := microvm.InitrdPath(filepath.Join(dataDir, "microvm"), initBin)
	if err != nil {
		return nil, err
	}
	maxVMs := 120
	if v, err := strconv.Atoi(os.Getenv("KVZ_MAX_VMS")); err == nil && v > 0 {
		maxVMs = v
	}
	store := image.NewStore(filepath.Join(home, ".kvz", "images"))
	// k3s-style registry mirrors/credentials (KVZ_REGISTRIES overrides the path).
	regPath := os.Getenv("KVZ_REGISTRIES")
	if regPath == "" {
		regPath = filepath.Join(home, ".kvz", "registries.yaml")
	}
	if err := store.LoadRegistries(regPath); err != nil {
		return nil, err
	}
	kernel := DefaultKernel()
	return &Backend{hostIP: hostIP, dir: filepath.Join(dataDir, "mvm"), kernel: kernel, kernelHotplug: kernelHasHotplug(kernel), kernelCgroups: kernelHas(kernel, "CONFIG_CFS_BANDWIDTH", "CONFIG_MEMCG"), initrd: initrd, t: t, vols: vols,
		images: store, pnet: pn, dns: []string{"10.43.0.10"},
		pods: map[string]*pod{}, pre: map[string]net.IP{}, slots: make(chan struct{}, maxVMs)}, nil
}

func (b *Backend) Handler() string { return "linux" }

// UpdatePod records the newest pod object for retries of a pod that is still
// waiting on its configuration.
func (b *Backend) UpdatePod(p *corev1.Pod) {
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps != nil {
		ps.latest.Store(p.DeepCopy())
		if b.kernelCgroups {
			b.resize(p, ps)
		}
		ps.nudge() // labels/annotations feed downwardAPI volumes
	}
}

func (ps *pod) nudge() {
	if ps.refresh != nil {
		select {
		case ps.refresh <- struct{}{}:
		default:
		}
	}
}

// RefreshVolumes re-renders the volumes of the namespace's pods (called on
// configMap/secret changes).
func (b *Backend) RefreshVolumes(ns string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ps := range b.pods {
		if ps.ns == ns {
			ps.nudge()
		}
	}
}

// Images exposes the store (pre-pull API).
func (b *Backend) Images() *image.Store { return b.images }

type cspec struct {
	Name       string      `json:"name"`
	Argv       []string    `json:"argv"`
	Env        []string    `json:"env"`
	Cwd        string      `json:"cwd"`
	UID        int         `json:"uid"`
	GID        int         `json:"gid"`
	ImageTag   string      `json:"image_tag"`
	Restart    string      `json:"restart"`
	Cgroup     *cgroupSpec `json:"cgroup,omitempty"`
	Sidecar    bool        `json:"sidecar,omitempty"`
	Groups     []int       `json:"groups,omitempty"`
	ReadOnly   bool        `json:"ro_root,omitempty"`
	TermPath   string      `json:"term_path,omitempty"`
	TermLogs   bool        `json:"term_fallback_logs,omitempty"`
	Privileged bool        `json:"privileged,omitempty"`
	CapAdd     []string    `json:"cap_add,omitempty"`
	CapDrop    []string    `json:"cap_drop,omitempty"`
	NoNewPrivs bool        `json:"no_new_privs,omitempty"`

	Readiness *probe   `json:"readiness,omitempty"`
	Liveness  *probe   `json:"liveness,omitempty"`
	Startup   *probe   `json:"startup,omitempty"`
	PostStart *handler `json:"post_start,omitempty"`
	PreStop   *handler `json:"pre_stop,omitempty"`
}

type mspec struct {
	Container string `json:"container"`
	Tag       string `json:"tag"`
	Sub       string `json:"sub"`
	Path      string `json:"path"`
	ReadOnly  bool   `json:"ro"`
	Tmpfs     string `json:"tmpfs,omitempty"` // memory-backed emptyDir name
	TmpfsSize int64  `json:"tmpfs_size,omitempty"`
}

type spec struct {
	Sysctls    map[string]string `json:"sysctls,omitempty"`
	CPUOnline  string            `json:"cpu_online,omitempty"`
	Search     []string          `json:"search"`
	MTU        int               `json:"mtu"`
	IP         string            `json:"ip"`
	Mask       int               `json:"mask"`
	GW         string            `json:"gw"`
	Hostname   string            `json:"hostname"`
	DNS        []string          `json:"dns"`
	Hosts      string            `json:"hosts"`
	Containers []cspec           `json:"containers"`
	Inits      []cspec           `json:"init_containers,omitempty"`
	Restart    string            `json:"restart"`
	Mounts     []mspec           `json:"mounts"`
}

// PrepareIP reserves the pod's IP early so status.podIP env refs resolve.
func (b *Backend) PrepareIP(p *corev1.Pod) string {
	key := rt.Key(p.Namespace, p.Name)
	b.mu.Lock()
	defer b.mu.Unlock()
	if ip, ok := b.pre[key]; ok {
		return ip.String()
	}
	if ps, ok := b.pods[key]; ok {
		return ps.ip.String()
	}
	ip, err := b.pnet.Alloc()
	if err != nil {
		return ""
	}
	b.pre[key] = ip
	return ip.String()
}

func (b *Backend) Start(ctx context.Context, p *corev1.Pod) error {
	key := rt.Key(p.Namespace, p.Name)
	b.mu.Lock()
	ip, ok := b.pre[key]
	delete(b.pre, key)
	b.mu.Unlock()
	if !ok {
		var err error
		if ip, err = b.pnet.Alloc(); err != nil {
			return err
		}
	}
	dir := filepath.Join(b.dir, string(p.UID))
	_ = os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	pctx, cancel := context.WithCancel(context.Background())
	ps := &pod{ip: ip, dir: dir, cancel: cancel, done: make(chan struct{}), ns: p.Namespace, refresh: make(chan struct{}, 1), first: p.DeepCopy()}
	b.mu.Lock()
	b.pods[key] = ps
	b.mu.Unlock()
	// Publish the IP immediately: it is fixed before boot. hostNetwork pods
	// report the node IP (their ports are forwarded from the host).
	statusIP := ip.String()
	if p.Spec.HostNetwork {
		statusIP = b.hostIP
	}
	b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
		s.PodIP = statusIP
		s.PodIPs = []corev1.PodIP{{IP: statusIP}}
	})
	go func() {
		defer close(ps.done)
		if err := b.run(pctx, p, ps); err != nil && pctx.Err() == nil {
			klog.ErrorS(err, "microvm pod failed", "pod", key)
			b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
				for _, c := range p.Spec.Containers {
					rt.SetTerminated(s, c.Name, 128, "StartError")
				}
				s.Message = err.Error()
			})
		}
	}()
	return nil
}

func (b *Backend) buildSpec(p *corev1.Pod, ps *pod) (*spec, []microvm.Share, error) {
	sp := &spec{Hostname: p.Name, DNS: b.dns,
		Search: []string{p.Namespace + ".svc.cluster.local", "svc.cluster.local", "cluster.local"}, IP: ps.ip.String(), Mask: b.pnet.Bits(), GW: b.pnet.GW.String(), MTU: b.pnet.MTU}
	if psc := p.Spec.SecurityContext; psc != nil && len(psc.Sysctls) > 0 {
		// every pod has its own kernel, so all sysctls are pod-scoped here
		sp.Sysctls = map[string]string{}
		for _, sc := range psc.Sysctls {
			sp.Sysctls[sc.Name] = sc.Value
		}
	}
	if p.Spec.Hostname != "" {
		sp.Hostname = p.Spec.Hostname
	}
	sp.Hosts = hostsFile(p, ps.ip.String(), sp.Hostname)
	shares := []microvm.Share{{Tag: "pod", Path: ps.dir}}
	seen := map[string]bool{}
	restart := string(p.Spec.RestartPolicy)
	if restart == "" {
		restart = "Always"
	}
	sp.Restart = restart
	build := func(c corev1.Container) (cspec, error) {
		im := ps.images[c.Image]
		if im == nil {
			return cspec{}, fmt.Errorf("image %s not resolved", c.Image)
		}
		if !seen[im.Tag] {
			shares = append(shares, microvm.Share{Tag: im.Tag, Path: im.Rootfs, ReadOnly: true})
			seen[im.Tag] = true
		}
		// $(VAR) references in command/args expand against the container's
		// env, as in the kubelet (image Entrypoint/Cmd are left alone).
		vars := map[string]string{}
		for _, e := range c.Env {
			vars[e.Name] = e.Value
		}
		mapping := expansion.MappingFuncFor(vars)
		expand := func(in []string) []string {
			out := make([]string, len(in))
			for i, a := range in {
				out[i] = expansion.Expand(a, mapping)
			}
			return out
		}
		argv := im.Entrypoint
		args := im.Cmd
		if len(c.Command) > 0 {
			argv, args = expand(c.Command), nil
		}
		if len(c.Args) > 0 {
			args = expand(c.Args)
		}
		argv = append(append([]string{}, argv...), args...)
		if len(argv) == 0 {
			return cspec{}, fmt.Errorf("container %s: no command", c.Name)
		}
		env := mergeEnv(im.Env, rt.Env(c))
		cwd := im.WorkingDir
		if c.WorkingDir != "" {
			cwd = c.WorkingDir
		}
		uid, gid := userIDs(im, c, p.Spec.SecurityContext)
		if nonRoot(c, p.Spec.SecurityContext) && uid == 0 {
			return cspec{}, fmt.Errorf("container has runAsNonRoot and image will run as root (pod: %q, container: %s)", p.Name, c.Name)
		}
		var groups []int
		if psc := p.Spec.SecurityContext; psc != nil {
			for _, g := range psc.SupplementalGroups {
				groups = append(groups, int(g))
			}
			if psc.FSGroup != nil {
				groups = append(groups, int(*psc.FSGroup))
			}
		}
		termPath := c.TerminationMessagePath
		if termPath == "" {
			termPath = "/dev/termination-log"
		}
		cs := cspec{Name: c.Name, Argv: argv, Env: env, Cwd: cwd, UID: uid, GID: gid, ImageTag: im.Tag, Restart: restart, Cgroup: cgroupFor(c.Resources),
			Groups: groups, ReadOnly: c.SecurityContext != nil && c.SecurityContext.ReadOnlyRootFilesystem != nil && *c.SecurityContext.ReadOnlyRootFilesystem,
			TermPath: termPath, TermLogs: c.TerminationMessagePolicy == corev1.TerminationMessageFallbackToLogsOnError,
			Readiness: convProbe(c, c.ReadinessProbe), Liveness: convProbe(c, c.LivenessProbe), Startup: convProbe(c, c.StartupProbe)}
		if sc := c.SecurityContext; sc != nil {
			cs.Privileged = sc.Privileged != nil && *sc.Privileged
			cs.NoNewPrivs = sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation
			if sc.Capabilities != nil {
				for _, a := range sc.Capabilities.Add {
					cs.CapAdd = append(cs.CapAdd, string(a))
				}
				for _, d := range sc.Capabilities.Drop {
					cs.CapDrop = append(cs.CapDrop, string(d))
				}
			}
		}
		if c.Lifecycle != nil {
			cs.PostStart = convHandler(c, c.Lifecycle.PostStart)
			cs.PreStop = convHandler(c, c.Lifecycle.PreStop)
		}
		return cs, nil
	}
	for _, c := range p.Spec.InitContainers {
		cs, err := build(c)
		if err != nil {
			return nil, nil, err
		}
		cs.Sidecar = c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		sp.Inits = append(sp.Inits, cs)
		ps.names = append(ps.names, c.Name)
	}
	for _, c := range p.Spec.Containers {
		cs, err := build(c)
		if err != nil {
			return nil, nil, err
		}
		sp.Containers = append(sp.Containers, cs)
		ps.names = append(ps.names, c.Name)
	}
	// Volumes: everything materialized under the pod dir is reachable through
	// the "kvz" share; hostPath gets its own share.
	mounts, err := b.vols.Materialize(p, ps.dir)
	if err != nil {
		return nil, nil, mountError{err}
	}
	hp := 0
	for cname, specs := range mounts {
		for _, m := range specs {
			parts := strings.Split(m, ":")
			host, guest := parts[0], parts[1]
			ro := len(parts) > 2 && parts[2] == "ro"
			if t, ok := strings.CutPrefix(host, volumes.TmpfsPrefix); ok {
				f := strings.SplitN(t, "/", 3) // volume, size, subpath
				size, _ := strconv.ParseInt(f[1], 10, 64)
				ms := mspec{Container: cname, Tmpfs: f[0], TmpfsSize: size, Path: guest, ReadOnly: ro}
				if len(f) > 2 {
					ms.Sub = f[2]
				}
				sp.Mounts = append(sp.Mounts, ms)
				continue
			}
			if rel, err := filepath.Rel(ps.dir, host); err == nil && !strings.HasPrefix(rel, "..") {
				sp.Mounts = append(sp.Mounts, mspec{Container: cname, Sub: rel, Path: guest, ReadOnly: ro})
			} else {
				tag := fmt.Sprintf("hp%d", hp)
				hp++
				shares = append(shares, microvm.Share{Tag: tag, Path: host, ReadOnly: ro})
				sp.Mounts = append(sp.Mounts, mspec{Container: cname, Tag: tag, Path: guest, ReadOnly: ro})
			}
		}
	}
	return sp, shares, nil
}

func mergeEnv(base, over []string) []string {
	idx := map[string]int{}
	out := append([]string{}, base...)
	for i, e := range out {
		idx[strings.SplitN(e, "=", 2)[0]] = i
	}
	for _, e := range over {
		k := strings.SplitN(e, "=", 2)[0]
		if i, ok := idx[k]; ok {
			out[i] = e
		} else {
			idx[k] = len(out)
			out = append(out, e)
		}
	}
	return out
}

func nonRoot(c corev1.Container, psc *corev1.PodSecurityContext) bool {
	if c.SecurityContext != nil && c.SecurityContext.RunAsNonRoot != nil {
		return *c.SecurityContext.RunAsNonRoot
	}
	return psc != nil && psc.RunAsNonRoot != nil && *psc.RunAsNonRoot
}

func userIDs(im *image.Image, c corev1.Container, psc *corev1.PodSecurityContext) (int, int) {
	var runAsUser, runAsGroup *int64
	if psc != nil {
		runAsUser, runAsGroup = psc.RunAsUser, psc.RunAsGroup
	}
	if c.SecurityContext != nil {
		if c.SecurityContext.RunAsUser != nil {
			runAsUser = c.SecurityContext.RunAsUser
		}
		if c.SecurityContext.RunAsGroup != nil {
			runAsGroup = c.SecurityContext.RunAsGroup
		}
	}
	if runAsUser != nil {
		gid := 0
		if runAsGroup != nil {
			gid = int(*runAsGroup)
		}
		return int(*runAsUser), gid
	}
	u := im.User
	if u == "" {
		if runAsGroup != nil {
			return 0, int(*runAsGroup)
		}
		return 0, 0
	}
	parts := strings.SplitN(u, ":", 2)
	uid, err := strconv.Atoi(parts[0])
	gid := 0
	if err != nil {
		uid, gid = lookupUser(im.Rootfs, parts[0])
	}
	if len(parts) == 2 {
		if g, err := strconv.Atoi(parts[1]); err == nil {
			gid = g
		}
	}
	return uid, gid
}

func lookupUser(root, name string) (int, int) {
	b, err := os.ReadFile(filepath.Join(root, "etc", "passwd"))
	if err != nil {
		return 0, 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 3 && f[0] == name {
			u, _ := strconv.Atoi(f[2])
			g, _ := strconv.Atoi(f[3])
			return u, g
		}
	}
	return 0, 0
}

// vcpus maps CPU limits to vCPUs. Like a cgroup without a quota, a pod with
// any container lacking a CPU limit may use every core of the node; vCPUs are
// host threads that cost nothing while idle.
func vcpus(p *corev1.Pod) uint {
	var milli int64
	for _, c := range p.Spec.Containers {
		q, ok := c.Resources.Limits[corev1.ResourceCPU]
		if !ok {
			return defaultVCPUs
		}
		milli += q.MilliValue()
	}
	n := uint((milli + 999) / 1000)
	if n < 1 {
		n = 1
	}
	if n > defaultVCPUs {
		n = defaultVCPUs
	}
	return n
}

var defaultMemMB = func() uint64 {
	if v, err := strconv.Atoi(os.Getenv("KVZ_DEFAULT_MEMORY_MB")); err == nil && v > 0 {
		return uint64(v)
	}
	// Pods without a memory limit may use node memory. Guest RAM is backed
	// lazily, but the guest kernel's memmap costs ~28 MB of host memory per GB
	// at boot, so the default is 2 GiB rather than all of the Mac's memory.
	return 2048
}()

// kernelHasHotplug reports whether the pod kernel can online CPUs at runtime
// (kernel/build.sh installs its .config next to the Image). Without it,
// maxcpus=1 would leave pods on one CPU, so the kernel boots them all.
func kernelHas(kernel string, opts ...string) bool {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(kernel), "config"))
	if err != nil {
		return false
	}
	for _, o := range opts {
		if !strings.Contains(string(b), "\n"+o+"=y\n") {
			return false
		}
	}
	return true
}

func kernelHasHotplug(kernel string) bool {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(kernel), "config"))
	return err == nil && strings.Contains(string(b), "\nCONFIG_HOTPLUG_CPU=y\n")
}

// cpuOnlineMode: "boot" (kernel brings all vCPUs up), "before" (kvz-init
// onlines them during rootfs/network setup, before containers start) or
// "after" (once containers run). KVZ_CPU_ONLINE overrides.
var cpuOnlineMode = func() string {
	if v := os.Getenv("KVZ_CPU_ONLINE"); v == "boot" || v == "before" || v == "after" {
		return v
	}
	return "before"
}()

var defaultVCPUs = func() uint {
	if v, err := strconv.Atoi(os.Getenv("KVZ_DEFAULT_VCPUS")); err == nil && v > 0 {
		return uint(v)
	}
	return uint(goruntime.NumCPU())
}()

func memMB(p *corev1.Pod) uint64 {
	var bytes int64
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			bytes += q.Value()
		}
	}
	mb := uint64(bytes >> 20)
	if mb == 0 {
		mb = defaultMemMB // only touched pages are backed by host memory
	}
	if mb < 128 {
		mb = 128
	}
	return mb
}

func (b *Backend) run(ctx context.Context, p *corev1.Pod, ps *pod) error {
	select {
	case b.slots <- struct{}{}:
	default:
		b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
			for _, c := range p.Spec.Containers {
				rt.SetWaiting(s, c.Name, "WaitingForVMSlot", fmt.Sprintf("node is at its microVM limit (%d)", cap(b.slots)))
			}
		})
		select {
		case b.slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
	}
	defer func() { <-b.slots }()
	if err := b.ensureImages(ctx, p, ps); err != nil {
		return nil // pod deleted while pulling
	}
	// Like the kubelet, configuration problems (missing configMaps/secrets,
	// runAsNonRoot violations) leave the pod waiting and are retried.
	var sp *spec
	var shares []microvm.Share
	for delay := 250 * time.Millisecond; ; delay = min(delay*2, 10*time.Second) {
		var err error
		if lp := ps.latest.Load(); lp != nil {
			p = lp // e.g. annotations a subPathExpr depends on were fixed
		}
		ps.names = ps.names[:0]
		if sp, shares, err = b.buildSpec(p, ps); err == nil {
			ps.resMu.Lock()
			ps.first, ps.applied = p.DeepCopy(), nil // resources the VM boots with
			ps.resMu.Unlock()
			break
		}
		reason, ev := "CreateContainerConfigError", "Failed"
		if _, ok := err.(mountError); ok {
			reason, ev = "ContainerCreating", "FailedMount"
		}
		b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
			for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
				rt.SetWaiting(s, c.Name, reason, err.Error())
			}
		})
		b.t.Event(p.Namespace, p.Name, corev1.EventTypeWarning, ev, "Error: %v", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	// SMP bring-up of many vCPUs in every booting guest is costly when pods
	// start in bulk; boot on one CPU and let kvz-init online the rest.
	ncpu, extraArgs := vcpus(p), ""
	mem := memMB(p)
	if b.kernelCgroups {
		// limits are enforced per container by cgroups: the VM gets every
		// vCPU (cpu.max does the limiting, like a cgroup quota on a node) and
		// memory headroom so in-place resizes up fit
		ncpu = defaultVCPUs
		mem = max(mem+256, defaultMemMB)
	}
	if ncpu > 1 && cpuOnlineMode != "boot" && b.kernelHotplug {
		sp.CPUOnline = cpuOnlineMode
		extraArgs = "maxcpus=1"
	}
	raw, _ := json.Marshal(sp)
	if err := os.WriteFile(filepath.Join(ps.dir, "spec.json"), raw, 0o644); err != nil {
		return err
	}
	mac := macFor(ps.ip)
	nic, err := b.pnet.AttachPod(ctx, ps.ip, mac)
	if err != nil {
		return err
	}
	vm, err := microvm.Boot(microvm.Config{Kernel: b.kernel, Initrd: b.initrd, IP: ps.ip, GW: b.pnet.GW, Mask: b.pnet.Bits(),
		CPUs: ncpu, MemoryMB: mem, ExtraArgs: extraArgs, Shares: shares, ConsoleLog: consoleLog(ps.dir), NetFile: nic, MTU: b.pnet.MTU, MAC: mac})
	if err != nil {
		nic.Close()
		return err
	}
	b.mu.Lock()
	ps.vm = vm
	b.mu.Unlock()
	if b.kernelCgroups { // a resize that arrived while the VM booted
		if lp := ps.latest.Load(); lp != nil {
			go b.resize(lp, ps)
		}
	}
	b.forwardHostPorts(p, ps)
	go func() { // keep configMap/secret/downwardAPI/projected volumes current, like the kubelet
		for {
			select {
			case <-ctx.Done():
				return
			case <-ps.refresh:
				time.Sleep(50 * time.Millisecond) // coalesce bursts; let informers settle
			case <-time.After(60 * time.Second):
			}
			cur := p
			if lp := ps.latest.Load(); lp != nil {
				cur = lp
			}
			b.vols.Refresh(cur, ps.dir)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-vm.Done():
			if ctx.Err() == nil {
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					for _, n := range ps.names {
						rt.SetTerminated(s, n, 137, "VMStopped")
					}
				})
			}
			return nil
		case e, ok := <-vm.Events:
			if !ok {
				<-vm.Done()
				continue
			}
			switch e.Type {
			case "started":
				c, isInit, sidecar := lookupContainer(p, e.Container)
				ready := c != nil && c.ReadinessProbe == nil && c.StartupProbe == nil && !isInit || sidecar && c.ReadinessProbe == nil && c.StartupProbe == nil
				id := fmt.Sprintf("kvz://%s/%d", p.UID, e.Pid)
				if sidecar {
					id = rt.SidecarIDPrefix + fmt.Sprintf("%s/%d", p.UID, e.Pid)
				}
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Created", "Created container %s", e.Container)
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Started", "Started container %s", e.Container)
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					for _, cs := range append(append([]corev1.ContainerStatus{}, s.ContainerStatuses...), s.InitContainerStatuses...) {
						if cs.Name == e.Container && (cs.State.Terminated != nil || cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff") {
							rt.IncRestart(s, e.Container)
						}
					}
					rt.SetRunning(s, e.Container, id, ready)
					// resources from the newest pod: a resize may already have
					// landed before this start event (status must not revert)
					cur := p
					if lp := ps.latest.Load(); lp != nil {
						cur = lp
					}
					if cc, _, _ := lookupContainer(cur, e.Container); cc != nil {
						setResources(s, e.Container, cc.Resources)
					}
				})
			case "ready", "unready":
				want := e.Type == "ready"
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) { rt.SetReady(s, e.Container, want) })
			case "init-failed":
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					rt.SetTerminated(s, e.Container, int32(e.Code), "Error")
					s.Phase = corev1.PodFailed
					rt.Recompute(s)
				})
			case "probe":
				klog.InfoS("container probe/hook", "pod", rt.Key(p.Namespace, p.Name), "container", e.Container, "msg", e.Msg)
				reason := "Unhealthy"
				if strings.HasPrefix(e.Msg, "postStart") {
					reason = "FailedPostStartHook"
				}
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeWarning, reason, "Container %s: %s", e.Container, e.Msg)
			case "exit":
				code := int32(e.Code)
				reason := "Completed"
				if code != 0 {
					reason = "Error"
				}
				if e.Reason != "" {
					reason = e.Reason // OOMKilled
				}
				_, isInit, sidecar := lookupContainer(p, e.Container)
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					switch {
					case ps.stopping.Load():
						rt.SetTerminated(s, e.Container, code, reason)
						rt.SetTerminationMessage(s, e.Container, e.Msg)
					case sidecar:
						rt.SetCrashLoop(s, e.Container, code, reason)
					case isInit && code == 0:
						rt.SetTerminated(s, e.Container, code, reason)
					case isInit:
						if p.Spec.RestartPolicy == corev1.RestartPolicyNever {
							rt.SetTerminated(s, e.Container, code, reason)
						} else {
							rt.SetCrashLoop(s, e.Container, code, reason)
						}
					case rt.WillRestart(p.Spec.RestartPolicy, code):
						rt.SetCrashLoop(s, e.Container, code, reason)
						if code != 0 {
							go b.t.Event(p.Namespace, p.Name, corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s", e.Container)
						}
					default:
						rt.SetTerminated(s, e.Container, code, reason)
					}
					rt.SetTerminationMessage(s, e.Container, e.Msg)
				})
			case "error":
				klog.InfoS("kvz-init error", "pod", rt.Key(p.Namespace, p.Name), "msg", e.Msg)
			}
		}
	}
}

func (b *Backend) Stop(ctx context.Context, p *corev1.Pod) error {
	key := rt.Key(p.Namespace, p.Name)
	b.mu.Lock()
	ps := b.pods[key]
	delete(b.pods, key)
	b.mu.Unlock()
	if ps == nil {
		return nil
	}
	ps.stopping.Store(true)
	for _, n := range ps.names {
		b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Killing", "Stopping container %s", n)
	}
	b.mu.Lock()
	vm := ps.vm
	b.mu.Unlock()
	t0 := time.Now()
	var tExit, tStop time.Duration
	defer func() {
		klog.InfoS("microvm pod stopped", "pod", key, "exit", tExit, "vmStop", tStop, "total", time.Since(t0))
	}()
	if vm != nil {
		grace := 1 * time.Second
		if p.Spec.TerminationGracePeriodSeconds != nil {
			grace = time.Duration(*p.Spec.TerminationGracePeriodSeconds) * time.Second
		}
		if grace > 0 && vm.Send(map[string]any{"type": "kill", "signal": int(syscall.SIGTERM), "grace": int(grace / time.Second)}) == nil {
			// wait until all containers exited or grace expires
			deadline := time.After(grace)
			exited := map[string]bool{}
			_ = exited
		wait:
			for {
				st := b.t.Get(p.Namespace, p.Name)
				all := st != nil
				if st != nil {
					for _, cs := range st.Status.ContainerStatuses {
						if cs.State.Terminated == nil {
							all = false
						}
					}
				}
				if all {
					break
				}
				select {
				case <-deadline:
					break wait
				case <-vm.Done():
					break wait
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		tExit = time.Since(t0)
		_ = vm.Stop()
		tStop = time.Since(t0)
	}
	for _, l := range ps.listeners {
		l.Close()
	}
	ps.cancel()
	<-ps.done
	b.pnet.Free(ps.ip)
	return os.RemoveAll(ps.dir)
}

func (b *Backend) Logs(ctx context.Context, p *corev1.Pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	return rt.ReadCRILog(ctx, filepath.Join(b.dir, string(p.UID), "logs", container+".log"), opts)
}

func (b *Backend) Exec(ctx context.Context, p *corev1.Pod, container string, cmd []string, attach api.AttachIO) error {
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps == nil || ps.vm == nil {
		return fmt.Errorf("pod not running")
	}
	return execInVM(ctx, ps.vm, container, cmd, attach)
}

// PortForward dials into the pod over the pod network.
func (b *Backend) PortForward(ctx context.Context, p *corev1.Pod, port int32, stream io.ReadWriteCloser) error {
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps == nil {
		return fmt.Errorf("pod not running")
	}
	c, err := b.pnet.Dial(ctx, net.JoinHostPort(ps.ip.String(), strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, stream); done <- struct{}{} }()
	go func() { io.Copy(stream, c); done <- struct{}{} }()
	<-done
	return nil
}

// macFor derives a pod's MAC from its IP (02:6b:<ip>). Pod IPs are reused
// constantly; with random MACs the long-lived gateway netstack kept stale ARP
// entries for a reused IP, so host->pod connections (port-forward, egress
// proxy, LoadBalancer) went to a MAC that no longer existed.
func macFor(ip net.IP) net.HardwareAddr {
	ip4 := ip.To4()
	return net.HardwareAddr{0x02, 0x6b, ip4[0], ip4[1], ip4[2], ip4[3]}
}

func lookupContainer(p *corev1.Pod, name string) (c *corev1.Container, isInit, sidecar bool) {
	for i := range p.Spec.InitContainers {
		if p.Spec.InitContainers[i].Name == name {
			ic := &p.Spec.InitContainers[i]
			return ic, true, ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways
		}
	}
	for i := range p.Spec.Containers {
		if p.Spec.Containers[i].Name == name {
			return &p.Spec.Containers[i], false, false
		}
	}
	return nil, false, false
}

// hostsFile renders /etc/hosts like the kubelet (hostname/subdomain FQDN, hostAliases).
func hostsFile(p *corev1.Pod, ip, hostname string) string {
	var b strings.Builder
	b.WriteString("# Kubernetes-managed hosts file.\n127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\nfe00::0\tip6-localnet\nfe00::0\tip6-mcastprefix\nfe00::1\tip6-allnodes\nfe00::2\tip6-allrouters\n")
	if p.Spec.Subdomain != "" {
		fmt.Fprintf(&b, "%s\t%s.%s.%s.svc.cluster.local\t%s\n", ip, hostname, p.Spec.Subdomain, p.Namespace, hostname)
	} else {
		fmt.Fprintf(&b, "%s\t%s\n", ip, hostname)
	}
	if len(p.Spec.HostAliases) > 0 {
		b.WriteString("\n# Entries added by HostAliases.\n")
		for _, a := range p.Spec.HostAliases {
			fmt.Fprintf(&b, "%s\t%s\n", a.IP, strings.Join(a.Hostnames, "\t"))
		}
	}
	return b.String()
}

type mountError struct{ error }

func (e mountError) Unwrap() error { return e.error }
