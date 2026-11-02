//go:build darwin

// vmbench: boot N kvz microVMs concurrently straight from the monitor (no k8s)
// and report per-phase timings. Isolates the VM path from the control plane.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/GrowlyX/kvz/pkg/image"
	"github.com/GrowlyX/kvz/pkg/microvm"
	"github.com/GrowlyX/kvz/pkg/podnet"
	"github.com/GrowlyX/kvz/pkg/runtime/microvmrt"
)

func main() {
	n := flag.Int("n", 1, "VMs")
	ref := flag.String("image", "docker.io/library/nginx:alpine", "image")
	kernel := flag.String("kernel", microvmrt.DefaultKernel(), "kernel")
	initBin := flag.String("init", "bin/kvz-init", "kvz-init binary (linux/arm64)")
	cpus := flag.Uint("cpus", 1, "vCPUs")
	mem := flag.Uint64("mem", 256, "memory MB")
	console := flag.Bool("console", false, "write console logs")
	hold := flag.Duration("hold", 0, "keep VMs running")
	base := flag.Int("ip-base", 200, "first host octet")
	extra := flag.String("args", "", "extra kernel args")
	usePodnet := flag.Bool("podnet", true, "use kvz podnet (false = VZ NAT)")
	noEnt := flag.Bool("no-entropy", false, "omit virtio-rng")
	flag.Parse()

	home, _ := os.UserHomeDir()
	work := filepath.Join(home, ".kvz-vmbench")
	_ = os.MkdirAll(work, 0o755)
	st := image.NewStore(filepath.Join(home, ".kvz", "images"))
	t := time.Now()
	im, err := st.Get(*ref)
	if err != nil {
		panic(err)
	}
	pullT := time.Since(t)
	ib, err := os.ReadFile(*initBin)
	if err != nil {
		panic(err)
	}
	initrd, err := microvm.InitrdPath(work, ib)
	if err != nil {
		panic(err)
	}
	argv := append(append([]string{}, im.Entrypoint...), im.Cmd...)
	var pn *podnet.Net
	if *usePodnet {
		pn, err = podnet.New("10.88.0.0/16", 0)
		if err != nil {
			panic(err)
		}
	}
	type res struct {
		cfg, create, start, init, hello, started time.Duration
		bootMs, readyMs                          float64
		err                                      string
	}
	out := make([]res, *n)
	var wg sync.WaitGroup
	vms := make([]*microvm.VM, *n)
	t0 := time.Now()
	for i := 0; i < *n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := filepath.Join(work, fmt.Sprintf("vm%d", i))
			_ = os.RemoveAll(dir)
			_ = os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
			ipS, gwS, mask := fmt.Sprintf("192.168.65.%d", *base+i), "192.168.65.1", 24
			var nic *os.File
			if pn != nil {
				ip, _ := pn.Alloc()
				ipS, gwS, mask = ip.String(), pn.GW.String(), pn.Bits()
				nic, _ = pn.Attach(context.Background())
			}
			spec := map[string]any{"hostname": fmt.Sprintf("vm%d", i), "dns": []string{gwS},
				"ip": ipS, "mask": mask, "gw": gwS,
				"containers": []map[string]any{{"name": "c", "argv": argv, "env": im.Env, "cwd": im.WorkingDir, "image_tag": im.Tag}}}
			b, _ := json.Marshal(spec)
			_ = os.WriteFile(filepath.Join(dir, "spec.json"), b, 0o644)
			s := time.Now()
			cl := ""
			if *console {
				cl = filepath.Join(dir, "console.log")
			}
			vm, err := microvm.Boot(microvm.Config{Kernel: *kernel, Initrd: initrd, CPUs: *cpus, MemoryMB: *mem,
				IP: net.ParseIP(fmt.Sprintf("192.168.65.%d", *base+i)), GW: net.ParseIP("192.168.65.1"), Mask: 24,
				Shares: []microvm.Share{{Tag: "pod", Path: dir}, {Tag: im.Tag, Path: im.Rootfs, ReadOnly: true}}, ConsoleLog: cl, ExtraArgs: *extra, NetFile: nic, NoEntropy: *noEnt})
			if err != nil {
				out[i].err = err.Error()
				return
			}
			vms[i] = vm
			out[i].cfg, out[i].create, out[i].start = vm.T.Configure, vm.T.Create, vm.T.Start
			timeout := time.After(30 * time.Second)
			for {
				select {
				case e, ok := <-vm.Events:
					if !ok {
						out[i].err = fmt.Sprintf("events closed: %v (state=%v)", vm.EventErr, vm.State())
						return
					}
					switch e.Type {
					case "hello":
						out[i].hello = time.Since(s)
						out[i].bootMs = e.BootMs
					case "started":
						out[i].init = vm.T.Init
						out[i].started = time.Since(s)
						out[i].readyMs = e.ReadyMs
						return
					case "error", "exit":
						out[i].err = e.Type + ": " + e.Msg
						if e.Type == "exit" {
							return
						}
					}
				case <-timeout:
					out[i].err = "timeout"
					return
				}
			}
		}()
	}
	wg.Wait()
	wall := time.Since(t0)
	var started []float64
	phase := map[string][]float64{}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	for i, r := range out {
		if r.err != "" {
			fmt.Printf("vm%d ERROR %s\n", i, r.err)
			continue
		}
		started = append(started, float64(r.started.Microseconds())/1000)
		if *n <= 4 || i < 2 {
			fmt.Printf("vm%d create=%v start=%v init=%v hello=%v started=%v | guest: init@%.0fms started@%.0fms\n",
				i, r.create.Round(time.Millisecond), r.start.Round(time.Millisecond), r.init.Round(time.Millisecond),
				r.hello.Round(time.Millisecond), r.started.Round(time.Millisecond), r.bootMs, r.readyMs)
		}
		phase["start"] = append(phase["start"], ms(r.start))
		phase["init"] = append(phase["init"], ms(r.init))
		phase["hello"] = append(phase["hello"], ms(r.hello))
	}
	sort.Float64s(started)
	if len(started) > 0 {
		fmt.Printf("n=%d ok=%d wall=%v p50=%.0fms max=%.0fms (image ready in %v)\n", *n, len(started), wall.Round(time.Millisecond),
			started[len(started)/2], started[len(started)-1], pullT.Round(time.Millisecond))
	}
	for _, k := range []string{"start", "init", "hello"} {
		v := phase[k]
		sort.Float64s(v)
		if len(v) > 0 {
			fmt.Printf("  %-6s p50=%.0fms max=%.0fms\n", k, v[len(v)/2], v[len(v)-1])
		}
	}
	if *hold > 0 && pn != nil {
		time.Sleep(300 * time.Millisecond) // let nginx bind
		// reachability check through the pod network
		for i := 0; i < *n && i < 3; i++ {
			c, err := pn.Dial(context.Background(), fmt.Sprintf("10.88.0.%d:80", 2+i))
			if err == nil {
				fmt.Fprintf(c, "GET / HTTP/1.0\r\n\r\n")
				buf := make([]byte, 64)
				nb, _ := c.Read(buf)
				fmt.Printf("dial 10.88.0.%d:80 -> %q\n", 2+i, string(buf[:nb]))
				c.Close()
			} else {
				fmt.Printf("dial 10.88.0.%d:80 -> %v\n", 2+i, err)
			}
		}
	}
	if *hold > 0 {
		time.Sleep(*hold)
	}
	var sw sync.WaitGroup
	for _, v := range vms {
		if v != nil {
			sw.Add(1)
			go func(v *microvm.VM) { defer sw.Done(); v.Stop() }(v)
		}
	}
	sw.Wait()
}
