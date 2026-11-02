// Package version holds the kvz release version, set at build time.
//
// A kvz release is named after the Kubernetes version it embeds plus a
// release number that increases with every release and never resets,
// for example v1.35.10-12. See docs/releasing.md.
package version

// Release is the kvz release, set with
// -ldflags "-X github.com/GrowlyX/kvz/pkg/version.Release=v1.35.10-12".
// Builds outside the release pipeline report a dev version.
var Release = "dev"

// Kubernetes is the upstream Kubernetes version kvz embeds.
const Kubernetes = "v1.35.10"

// String is the line printed by `kvz version`.
func String() string {
	return "kvz " + Release + " (Kubernetes " + Kubernetes + ")"
}
