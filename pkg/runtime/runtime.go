// Package runtime defines the pod backend interface used by the kvz node and
// a small status tracker shared by the backends.
package runtime

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

// Backend runs pods of one RuntimeClass handler.
type Backend interface {
	Handler() string
	// Start launches the pod asynchronously; the backend reports progress via
	// the Tracker it was constructed with.
	Start(ctx context.Context, pod *corev1.Pod) error
	Stop(ctx context.Context, pod *corev1.Pod) error
	Logs(ctx context.Context, pod *corev1.Pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error)
	Exec(ctx context.Context, pod *corev1.Pod, container string, cmd []string, attach api.AttachIO) error
}

func Key(ns, name string) string { return ns + "/" + name }

// Tracker stores the provider's view of every pod and pushes changes to the
// virtual-kubelet notifier so status changes are reported without polling.
type Tracker struct {
	mu     sync.RWMutex
	pods   map[string]*corev1.Pod
	notify func(*corev1.Pod)
	// Recorder emits kubelet-style pod events (set by the node).
	Recorder record.EventRecorder
}

// Event records a pod event (Normal/Warning) like the kubelet does.
func (t *Tracker) Event(ns, name, typ, reason, msgFmt string, args ...any) {
	if t.Recorder == nil {
		return
	}
	if p := t.Get(ns, name); p != nil {
		t.Recorder.Eventf(p, typ, reason, msgFmt, args...)
	}
}

func NewTracker() *Tracker { return &Tracker{pods: map[string]*corev1.Pod{}} }

func (t *Tracker) SetNotifier(f func(*corev1.Pod)) {
	t.mu.Lock()
	t.notify = f
	t.mu.Unlock()
}

func (t *Tracker) Put(pod *corev1.Pod) {
	t.mu.Lock()
	t.pods[Key(pod.Namespace, pod.Name)] = pod
	t.mu.Unlock()
}

func (t *Tracker) Get(ns, name string) *corev1.Pod {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p, ok := t.pods[Key(ns, name)]; ok {
		return p.DeepCopy()
	}
	return nil
}

func (t *Tracker) Delete(ns, name string) {
	t.mu.Lock()
	delete(t.pods, Key(ns, name))
	t.mu.Unlock()
}

func (t *Tracker) List() []*corev1.Pod {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(t.pods))
	for _, p := range t.pods {
		out = append(out, p.DeepCopy())
	}
	return out
}

// Update mutates the stored pod's status and notifies virtual-kubelet.
func (t *Tracker) Update(ns, name string, f func(*corev1.PodStatus)) {
	t.mu.Lock()
	p, ok := t.pods[Key(ns, name)]
	if !ok {
		t.mu.Unlock()
		return
	}
	f(&p.Status)
	cp := p.DeepCopy()
	n := t.notify
	t.mu.Unlock()
	if n != nil {
		n(cp)
	}
}

// --- status helpers -------------------------------------------------------

func now() metav1.Time { return metav1.NewTime(time.Now()) }

// SidecarIDPrefix marks restartable init containers (native sidecars) in
// their ContainerID so Recompute can tell them apart from regular init
// containers.
const SidecarIDPrefix = "kvz-sidecar://"

// InitStatus resets the pod to Pending with waiting containers.
func InitStatus(pod *corev1.Pod, hostIP string) {
	t := now()
	initialized := corev1.ConditionTrue
	waitReason := "ContainerCreating"
	if len(pod.Spec.InitContainers) > 0 {
		initialized = corev1.ConditionFalse
		waitReason = "PodInitializing"
	}
	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodPending,
		HostIP:    hostIP,
		HostIPs:   []corev1.HostIP{{IP: hostIP}},
		StartTime: &t,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: t},
			{Type: corev1.PodInitialized, Status: initialized, LastTransitionTime: t},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: t},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: t},
		},
	}
	for _, c := range pod.Spec.InitContainers {
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, Image: c.Image, ImageID: c.Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		})
	}
	for _, c := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, Image: c.Image, ImageID: c.Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitReason}},
		})
	}
}

func setCond(s *corev1.PodStatus, typ corev1.PodConditionType, v corev1.ConditionStatus) {
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			if s.Conditions[i].Status != v {
				s.Conditions[i].Status = v
				s.Conditions[i].LastTransitionTime = now()
			}
			return
		}
	}
	s.Conditions = append(s.Conditions, corev1.PodCondition{Type: typ, Status: v, LastTransitionTime: now()})
}

// find returns the status of an app or init container.
func find(s *corev1.PodStatus, name string) *corev1.ContainerStatus {
	for i := range s.ContainerStatuses {
		if s.ContainerStatuses[i].Name == name {
			return &s.ContainerStatuses[i]
		}
	}
	for i := range s.InitContainerStatuses {
		if s.InitContainerStatuses[i].Name == name {
			return &s.InitContainerStatuses[i]
		}
	}
	return nil
}

// SetRunning marks a container running and recomputes pod phase/readiness.
func SetRunning(s *corev1.PodStatus, container, containerID string, ready bool) {
	if cs := find(s, container); cs != nil {
		if cs.State.Running == nil {
			cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now()}}
		}
		cs.ContainerID = containerID
		cs.Ready = ready
		started := true
		cs.Started = &started
	}
	Recompute(s)
}

// SetReady flips a running container's readiness (readiness probe result).
func SetReady(s *corev1.PodStatus, container string, ready bool) {
	if cs := find(s, container); cs != nil && cs.State.Running != nil {
		cs.Ready = ready
	}
	Recompute(s)
}

// SetTerminated marks a container exited.
func SetTerminated(s *corev1.PodStatus, container string, code int32, reason string) {
	if cs := find(s, container); cs != nil {
		var started metav1.Time
		if cs.State.Running != nil {
			started = cs.State.Running.StartedAt
		}
		cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, StartedAt: started, FinishedAt: now()}}
		cs.Ready = false
	}
	Recompute(s)
}

func SetWaiting(s *corev1.PodStatus, container, reason, msg string) {
	if cs := find(s, container); cs != nil {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
		cs.Ready = false
	}
	Recompute(s)
}

func IncRestart(s *corev1.PodStatus, container string) {
	if cs := find(s, container); cs != nil {
		cs.RestartCount++
	}
}

// IsInit reports whether name is an init container of the pod status.
func IsInit(s *corev1.PodStatus, name string) bool {
	for _, cs := range s.InitContainerStatuses {
		if cs.Name == name {
			return true
		}
	}
	return false
}

func isSidecar(cs corev1.ContainerStatus) bool {
	return len(cs.ContainerID) >= len(SidecarIDPrefix) && cs.ContainerID[:len(SidecarIDPrefix)] == SidecarIDPrefix
}

// Recompute derives phase + Initialized/Ready conditions from container
// states, like the kubelet: init containers (except sidecars) must all have
// succeeded before the pod is Initialized; sidecars count toward readiness.
func Recompute(s *corev1.PodStatus) {
	initDone, initFailed := true, false
	sidecars, sidecarsReady := 0, 0
	for _, cs := range s.InitContainerStatuses {
		switch {
		case isSidecar(cs) && cs.State.Running != nil:
			sidecars++
			if cs.Ready {
				sidecarsReady++
			}
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
		case cs.State.Terminated != nil:
			initDone, initFailed = false, true
		default:
			initDone = false
		}
	}
	if initDone {
		setCond(s, corev1.PodInitialized, corev1.ConditionTrue)
	} else {
		setCond(s, corev1.PodInitialized, corev1.ConditionFalse)
	}
	all, running, ready, failed, succeeded := len(s.ContainerStatuses), 0, 0, 0, 0
	for _, cs := range s.ContainerStatuses {
		switch {
		case cs.State.Running != nil:
			running++
			if cs.Ready {
				ready++
			}
		case cs.State.Terminated != nil:
			if cs.State.Terminated.ExitCode == 0 {
				succeeded++
			} else {
				failed++
			}
		}
	}
	switch {
	case initFailed && s.Phase == corev1.PodFailed:
	case !initDone:
		// still initializing (or an init container is being retried)
	case all > 0 && succeeded == all:
		s.Phase = corev1.PodSucceeded
	case all > 0 && failed+succeeded == all && failed > 0:
		s.Phase = corev1.PodFailed
	case running > 0:
		s.Phase = corev1.PodRunning
	case s.Phase == corev1.PodRunning:
		// containers in CrashLoopBackOff keep the pod Running
	}
	r := corev1.ConditionFalse
	if initDone && all > 0 && ready == all && sidecarsReady == sidecars {
		r = corev1.ConditionTrue
	}
	setCond(s, corev1.PodReady, r)
	setCond(s, corev1.ContainersReady, r)
}

// Env flattens literal container env vars (valueFrom is resolved by
// virtual-kubelet's downward-API pass before CreatePod).
func Env(c corev1.Container) []string {
	out := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}

// SetCrashLoop records an exit of a container that will be restarted
// (restartPolicy Always, or OnFailure with a non-zero code): the pod stays
// Running, the container waits in CrashLoopBackOff, lastState keeps the exit.
func SetCrashLoop(s *corev1.PodStatus, container string, code int32, reason string) {
	if cs := find(s, container); cs != nil {
		var started metav1.Time
		if cs.State.Running != nil {
			started = cs.State.Running.StartedAt
		}
		cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, StartedAt: started, FinishedAt: now()}}
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: fmt.Sprintf("back-off restarting failed container %s", container)}}
		cs.Ready = false
	}
	Recompute(s)
}

// WillRestart reports whether restartPolicy restarts a container that exited with code.
func WillRestart(policy corev1.RestartPolicy, code int32) bool {
	switch policy {
	case corev1.RestartPolicyNever:
		return false
	case corev1.RestartPolicyOnFailure:
		return code != 0
	}
	return true
}

// ContainerStats / PodStats are cumulative CPU time and current memory.
type ContainerStats struct {
	CPUNs    uint64
	MemBytes uint64
}

type PodStats struct {
	CPUNs      uint64
	MemBytes   uint64
	Containers map[string]ContainerStats
}
