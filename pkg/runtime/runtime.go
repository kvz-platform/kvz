// Package runtime defines the pod backend interface used by the kvz node and
// a small status tracker shared by the backends.
package runtime

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// InitStatus resets the pod to Pending with waiting containers.
func InitStatus(pod *corev1.Pod, hostIP string) {
	t := now()
	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodPending,
		HostIP:    hostIP,
		HostIPs:   []corev1.HostIP{{IP: hostIP}},
		StartTime: &t,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: t},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: t},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: t},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: t},
		},
	}
	for _, c := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, Image: c.Image, ImageID: c.Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
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

// SetRunning marks a container running and recomputes pod phase/readiness.
func SetRunning(s *corev1.PodStatus, container, containerID string, ready bool) {
	for i := range s.ContainerStatuses {
		cs := &s.ContainerStatuses[i]
		if cs.Name != container {
			continue
		}
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

// SetTerminated marks a container exited.
func SetTerminated(s *corev1.PodStatus, container string, code int32, reason string) {
	for i := range s.ContainerStatuses {
		cs := &s.ContainerStatuses[i]
		if cs.Name != container {
			continue
		}
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
	for i := range s.ContainerStatuses {
		cs := &s.ContainerStatuses[i]
		if cs.Name == container {
			cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
			cs.Ready = false
		}
	}
	Recompute(s)
}

func IncRestart(s *corev1.PodStatus, container string) {
	for i := range s.ContainerStatuses {
		if s.ContainerStatuses[i].Name == container {
			s.ContainerStatuses[i].RestartCount++
		}
	}
}

// Recompute derives phase + Ready conditions from container states.
func Recompute(s *corev1.PodStatus) {
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
	case all > 0 && succeeded == all:
		s.Phase = corev1.PodSucceeded
	case all > 0 && failed+succeeded == all && failed > 0:
		s.Phase = corev1.PodFailed
	case running > 0:
		s.Phase = corev1.PodRunning
	}
	r := corev1.ConditionFalse
	if all > 0 && ready == all {
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
