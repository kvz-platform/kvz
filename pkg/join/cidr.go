package join

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CIDRAllocator hands each node its own /16 pod CIDR (10.88.0.0/16 for the
// first node, then 10.89, 10.90, ...). Allocations live in the
// kube-system/kvz-podcidrs ConfigMap so every server of an HA cluster agrees;
// rejoining nodes keep theirs.
type CIDRAllocator struct {
	cs     kubernetes.Interface
	legacy string // pre-HA podcidrs.json, imported once
	mu     sync.Mutex
}

const cidrConfigMap = "kvz-podcidrs"

func NewCIDRAllocator(cs kubernetes.Interface, legacyPath string) *CIDRAllocator {
	return &CIDRAllocator{cs: cs, legacy: legacyPath}
}

func cidrOf(i int) string { return fmt.Sprintf("10.%d.0.0/16", 88+i) }

// For returns node's pod CIDR, allocating one if needed.
func (a *CIDRAllocator) For(node string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cms := a.cs.CoreV1().ConfigMaps("kube-system")
	for attempt := 0; ; attempt++ {
		cm, err := cms.Get(ctx, cidrConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cidrConfigMap, Namespace: "kube-system"}, Data: map[string]string{}}
			var old map[string]int
			if b, err := os.ReadFile(a.legacy); err == nil && json.Unmarshal(b, &old) == nil {
				for n, i := range old {
					cm.Data[n] = strconv.Itoa(i)
				}
			}
			if cm, err = cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				return "", err
			}
			continue
		} else if err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if v, ok := cm.Data[node]; ok {
			i, _ := strconv.Atoi(v)
			return cidrOf(i), nil
		}
		used := map[int]bool{}
		for _, v := range cm.Data {
			i, _ := strconv.Atoi(v)
			used[i] = true
		}
		i := 0
		for used[i] {
			i++
		}
		if i > 39 { // 10.88 .. 10.127
			return "", fmt.Errorf("pod CIDR space exhausted")
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[node] = strconv.Itoa(i)
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err == nil {
			return cidrOf(i), nil
		} else if !apierrors.IsConflict(err) || attempt > 20 {
			return "", err
		}
	}
}

// OverlaySecret derives the cross-node link secret from the join token.
func OverlaySecret(token string) []byte {
	if i := strings.Index(token, "::"); i >= 0 {
		token = token[i+2:]
	}
	s := sha256.Sum256([]byte("kvz-overlay:" + token))
	return s[:]
}
