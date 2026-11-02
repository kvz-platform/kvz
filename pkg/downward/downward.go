// Package downward resolves downward-API resource fields like the kubelet:
// unset limits default to the node's allocatable resources.
package downward

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/api/v1/resource"
)

var (
	mu          sync.RWMutex
	allocatable corev1.ResourceList
)

// SetNodeAllocatable records the node's allocatable resources.
func SetNodeAllocatable(r corev1.ResourceList) {
	mu.Lock()
	allocatable = r.DeepCopy()
	mu.Unlock()
}

// Resource returns the value of a resourceFieldRef for a container.
func Resource(fs *corev1.ResourceFieldSelector, pod *corev1.Pod, container string) (string, error) {
	mu.RLock()
	alloc := allocatable
	mu.RUnlock()
	if fs.ContainerName != "" {
		container = fs.ContainerName
	}
	return resource.ExtractResourceValueByContainerNameAndNodeAllocatable(fs, pod, container, alloc)
}
