// cpubench: identical CPU/memory micro-benchmark for host vs guest comparison.
// Prints one JSON line: sha256 MB/s (1 thread, all threads), memcpy GB/s.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

func shaMBps(threads int, d time.Duration) float64 {
	buf := make([]byte, 64<<10)
	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	deadline := time.Now().Add(d)
	t0 := time.Now()
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for time.Now().Before(deadline) {
				for j := 0; j < 64; j++ {
					sha256.Sum256(buf)
					n += len(buf)
				}
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	return float64(total) / time.Since(t0).Seconds() / 1e6
}

func memcpyGBps(d time.Duration) float64 {
	a, b := make([]byte, 256<<20), make([]byte, 256<<20)
	for i := range a {
		a[i] = byte(i)
	}
	n := 0
	t0 := time.Now()
	for time.Since(t0) < d {
		copy(b, a)
		n += len(a)
	}
	return float64(n) / time.Since(t0).Seconds() / 1e9
}

func main() {
	where := "unknown"
	if len(os.Args) > 1 {
		where = os.Args[1]
	}
	n := runtime.NumCPU()
	r := map[string]any{
		"suite": "cpubench", "where": where, "ncpu": n,
		"sha256_1t_MBps":  round(shaMBps(1, 5*time.Second)),
		"sha256_all_MBps": round(shaMBps(n, 5*time.Second)),
		"memcpy_GBps":     round(memcpyGBps(3 * time.Second)),
	}
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

func round(x float64) float64 { return float64(int(x*10)) / 10 }
