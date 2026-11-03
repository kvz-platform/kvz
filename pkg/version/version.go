// Package version holds the kvz release version, set at build time.
//
// A kvz release is named after the Kubernetes version it embeds plus a
// release number that increases with every release and never resets,
// for example v1.37.2-12. See docs/releasing.md.
package version

// Release is the kvz release, set with
// -ldflags "-X github.com/GrowlyX/kvz/pkg/version.Release=v1.37.2-12".
// Builds outside the release pipeline report a dev version.
var Release = "dev"

// Kubernetes is the upstream Kubernetes version kvz embeds. It must match
// K8S_VERSION in the Makefile and k8s.io/kubernetes in go.mod (version_test.go
// checks both).
const Kubernetes = "v1.37.2"

// Kubelet is the version kvz nodes report as their kubelet version.
const Kubelet = Kubernetes + "+kvz"

// String is the line printed by `kvz version`.
func String() string {
	return "kvz " + Release + " (Kubernetes " + Kubernetes + ")"
}
