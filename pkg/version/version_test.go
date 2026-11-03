package version

import (
	"os"
	"regexp"
	"testing"
)

// The embedded Kubernetes version is written in three places; a Kubernetes
// upgrade has to change all of them together.
func TestKubernetesVersionMatchesBuild(t *testing.T) {
	for _, c := range []struct{ file, pattern string }{
		{"../../Makefile", `(?m)^K8S_VERSION \?= (v\S+)$`},
		{"../../go.mod", `(?m)^\s+k8s.io/kubernetes (v\S+)$`},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(c.pattern).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no Kubernetes version found", c.file)
		}
		if got := string(m[1]); got != Kubernetes {
			t.Errorf("%s has Kubernetes %s, pkg/version has %s", c.file, got, Kubernetes)
		}
	}
}
