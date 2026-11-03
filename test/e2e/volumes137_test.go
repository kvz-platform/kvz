//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A self-signed CA certificate (key discarded), used only as trust-bundle
// content: the API server checks that trust bundles parse as certificates.
const testCAPEM = `-----BEGIN CERTIFICATE-----
MIIBWzCCAQCgAwIBAgIUCzx0C24yXns7XRfQ0U8zZl5y854wCgYIKoZIzj0EAwIw
EjEQMA4GA1UEAwwHa3Z6LWUyZTAgFw0yNjExMDMwMTMzMzZaGA8yMTI2MTAxMDAx
MzMzNlowEjEQMA4GA1UEAwwHa3Z6LWUyZTBZMBMGByqGSM49AgEGCCqGSM49AwEH
A0IABNSasOlKZLkpwiTu+R5nIvE6gQGu7AEBJvhT+WF6z5tB3Y6Bzu7oqg/zqzAq
Hhf+7nlc1+SImMQ8Df9VCUB0CbijMjAwMB0GA1UdDgQWBBRu+ktpEYCNA9D0vexI
S9KXvTmaVjAPBgNVHRMBAf8EBTADAQH/MAoGCCqGSM49BAMCA0kAMEYCIQDZDY3O
/zb14970jsxTZebNE3qHAxoCOfGLUKhTqHuy5wIhAI1pbvvtathitsjrR1u6vEU4
O+PgfW8PaFT8X6MqQNCC
-----END CERTIFICATE-----`

// Image volumes (spec.volumes[].image) and clusterTrustBundle projected
// volumes, both part of the Kubernetes 1.37 conformance profile.
func TestImageAndTrustBundleVolumes(t *testing.T) {
	ctb := "apiVersion: certificates.k8s.io/v1\nkind: ClusterTrustBundle\nmetadata: {name: kvz-e2e-ctb}\nspec:\n  trustBundle: |\n" +
		"    " + strings.ReplaceAll(testCAPEM, "\n", "\n    ") + "\n"
	apply(t, ctb)
	t.Cleanup(func() { _, _ = kubectl("delete", "clustertrustbundle", "kvz-e2e-ctb", "--ignore-not-found") })
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-vols137}
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: alpine:3.22
    command: [sh, -c, 'test -x /img/bin/busybox && echo img-content; touch /img/x 2>/dev/null && echo img-rw || echo img-ro; grep -q "BEGIN CERTIFICATE" /ctb/ca.pem && echo ctb-ok']
    volumeMounts:
    - {name: img, mountPath: /img}
    - {name: ctb, mountPath: /ctb}
  volumes:
  - {name: img, image: {reference: 'busybox:1.37', pullPolicy: IfNotPresent}}
  - name: ctb
    projected: {sources: [{clusterTrustBundle: {name: kvz-e2e-ctb, path: ca.pem}}]}
`)
	t.Cleanup(func() { _, _ = kubectl("delete", "pod", "e2e-vols137", "--now", "--ignore-not-found") })
	out := podLogs(t, "e2e-vols137")
	for _, want := range []string{"img-content", "img-ro", "ctb-ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}
