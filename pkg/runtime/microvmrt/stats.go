//go:build darwin

package microvmrt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"

	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

type bufIO struct{ b bytes.Buffer }

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (x *bufIO) TTY() bool                   { return false }
func (x *bufIO) Stdin() io.Reader            { return nil }
func (x *bufIO) Stdout() io.WriteCloser      { return nopCloser{&x.b} }
func (x *bufIO) Stderr() io.WriteCloser      { return nil }
func (x *bufIO) Resize() <-chan api.TermSize { return nil }

// Stats returns per-container CPU/memory for a running pod VM.
func (b *Backend) Stats(ctx context.Context, p *corev1.Pod) (*rt.PodStats, error) {
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps == nil || ps.vm == nil {
		return nil, nil
	}
	var io bufIO
	if err := execInVM(ctx, ps.vm, "", []string{"__kvz_stats"}, &io); err != nil {
		return nil, err
	}
	var raw struct {
		TimeNs     int64 `json:"time_ns"`
		Containers map[string]struct {
			CPUNs    uint64 `json:"cpu_ns"`
			MemBytes uint64 `json:"mem_bytes"`
		} `json:"containers"`
		VMMemBytes uint64 `json:"vm_mem_bytes"`
		VMCPUNs    uint64 `json:"vm_cpu_ns"`
	}
	if err := json.Unmarshal(io.b.Bytes(), &raw); err != nil {
		return nil, err
	}
	out := &rt.PodStats{CPUNs: raw.VMCPUNs, MemBytes: raw.VMMemBytes, Containers: map[string]rt.ContainerStats{}}
	for n, c := range raw.Containers {
		out.Containers[n] = rt.ContainerStats{CPUNs: c.CPUNs, MemBytes: c.MemBytes}
	}
	return out, nil
}
