package join

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// CIDRAllocator hands each node its own /16 pod CIDR (10.88.0.0/16 for the
// first node, then 10.89, 10.90, ...), persisted so rejoining nodes keep theirs.
type CIDRAllocator struct {
	path string
	mu   sync.Mutex
	m    map[string]int
}

func NewCIDRAllocator(path string) *CIDRAllocator {
	a := &CIDRAllocator{path: path, m: map[string]int{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &a.m)
	}
	return a
}

func (a *CIDRAllocator) For(node string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	i, ok := a.m[node]
	if !ok {
		used := map[int]bool{}
		for _, v := range a.m {
			used[v] = true
		}
		for used[i] {
			i++
		}
		a.m[node] = i
		b, _ := json.Marshal(a.m)
		_ = os.WriteFile(a.path, b, 0o600)
	}
	return fmt.Sprintf("10.%d.0.0/16", 88+i)
}

// OverlaySecret derives the cross-node link secret from the join token.
func OverlaySecret(token string) []byte {
	if i := strings.Index(token, "::"); i >= 0 {
		token = token[i+2:]
	}
	s := sha256.Sum256([]byte("kvz-overlay:" + token))
	return s[:]
}
