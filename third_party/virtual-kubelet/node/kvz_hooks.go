package node

import (
	"github.com/virtual-kubelet/virtual-kubelet/internal/podutils"
	corev1 "k8s.io/api/core/v1"
)

// SetResourceFieldRefResolver installs the resolver for resourceFieldRef
// environment variables (kvz patch).
func SetResourceFieldRefResolver(f func(fs *corev1.ResourceFieldSelector, pod *corev1.Pod, c *corev1.Container) (string, error)) {
	podutils.ResolveResourceFieldRef = f
}
