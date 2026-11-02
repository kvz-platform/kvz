package microvmrt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// cgroupSpec is a container's cgroup v2 configuration (see kvz-init).
type cgroupSpec struct {
	CPUMax    string `json:"cpu_max,omitempty"`
	CPUWeight int    `json:"cpu_weight,omitempty"`
	MemoryMax string `json:"memory_max,omitempty"`
}

const cfsPeriod = 100000 // µs, as the kubelet uses

// cgroupFor maps container resources to cgroup values the way the kubelet
// does: cpu.max quota from limits.cpu (min 1ms), cpu.weight from the CPU
// request via shares (requests default to limits), memory.max from limits.memory.
func cgroupFor(r corev1.ResourceRequirements) *cgroupSpec {
	g := &cgroupSpec{CPUMax: fmt.Sprintf("max %d", cfsPeriod), MemoryMax: "max"}
	if q, ok := r.Limits[corev1.ResourceCPU]; ok && !q.IsZero() {
		quota := q.MilliValue() * cfsPeriod / 1000
		if quota < 1000 {
			quota = 1000
		}
		g.CPUMax = fmt.Sprintf("%d %d", quota, cfsPeriod)
	}
	req, ok := r.Requests[corev1.ResourceCPU]
	if !ok {
		req, ok = r.Limits[corev1.ResourceCPU]
	}
	shares := int64(2) // kubelet minimum for pods without a request
	if ok {
		if shares = req.MilliValue() * 1024 / 1000; shares < 2 {
			shares = 2
		}
	}
	if shares > 262144 {
		shares = 262144
	}
	g.CPUWeight = int(1 + ((shares-2)*9999)/262142)
	if q, ok := r.Limits[corev1.ResourceMemory]; ok && !q.IsZero() {
		g.MemoryMax = fmt.Sprintf("%d", q.Value())
	}
	return g
}

// setResources reports the resources a container runs with (status.resources
// and allocatedResources, which in-place resize clients watch).
func setResources(s *corev1.PodStatus, container string, r corev1.ResourceRequirements) {
	for _, list := range [][]corev1.ContainerStatus{s.ContainerStatuses, s.InitContainerStatuses} {
		for i := range list {
			if list[i].Name != container {
				continue
			}
			rr := r.DeepCopy()
			list[i].Resources = rr
			list[i].AllocatedResources = rr.Requests.DeepCopy()
		}
	}
}

// resize applies changed container resources of a running pod (pods/resize):
// new cgroup limits in place, or a container restart where resizePolicy
// says RestartContainer.
func (b *Backend) resize(p *corev1.Pod, ps *pod) {
	b.mu.Lock()
	vm := ps.vm
	b.mu.Unlock()
	if vm == nil {
		return
	}
	ps.resMu.Lock()
	defer ps.resMu.Unlock()
	if ps.applied == nil { // baseline: what the pod booted with
		ps.applied = map[string]corev1.ResourceRequirements{}
		for _, c := range ps.first.Spec.Containers {
			ps.applied[c.Name] = c.Resources
		}
	}
	for _, c := range p.Spec.Containers {
		old := ps.applied[c.Name]
		if equality.Semantic.DeepEqual(old, c.Resources) {
			continue
		}
		restart := false
		for _, rp := range c.ResizePolicy {
			if rp.RestartPolicy != corev1.RestartContainer {
				continue
			}
			if !old.Requests[rp.ResourceName].Equal(c.Resources.Requests[rp.ResourceName]) ||
				!old.Limits[rp.ResourceName].Equal(c.Resources.Limits[rp.ResourceName]) {
				restart = true
			}
		}
		ps.applied[c.Name] = c.Resources
		_ = vm.Send(map[string]any{"type": "resize", "container": c.Name, "cgroup": cgroupFor(c.Resources)})
		if restart {
			_ = vm.Send(map[string]any{"type": "restart-container", "container": c.Name})
		}
		res := c.Resources
		name := c.Name
		b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) { setResources(s, name, res) })
		b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "ResizeCompleted", "Resized container %s (restart: %v)", c.Name, restart)
	}
}
