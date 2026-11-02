//go:build linux

package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// statsJSON reports per-container CPU time and resident memory, aggregated
// over each container's session (every container is started with setsid, so
// its processes share session id = container pid), plus VM totals.
func statsJSON() []byte {
	pmu.Lock()
	sess := map[int]string{}
	for pid, name := range pids {
		sess[pid] = name
	}
	pmu.Unlock()
	type cs struct {
		CPUNs    uint64 `json:"cpu_ns"`
		MemBytes uint64 `json:"mem_bytes"`
	}
	out := struct {
		TimeNs     int64         `json:"time_ns"`
		Containers map[string]cs `json:"containers"`
		VMMemBytes uint64        `json:"vm_mem_bytes"`
		VMCPUNs    uint64        `json:"vm_cpu_ns"`
	}{TimeNs: time.Now().UnixNano(), Containers: map[string]cs{}}
	const tick = 10_000_000 // USER_HZ=100 -> 10ms per tick
	page := uint64(os.Getpagesize())
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// fields after the ")" of comm: state(3) ppid pgrp session ... utime(14) stime(15) ... rss(24)
		r := strings.LastIndexByte(string(b), ')')
		if r < 0 {
			continue
		}
		f := strings.Fields(string(b[r+1:]))
		if len(f) < 22 {
			continue
		}
		session, _ := strconv.Atoi(f[3])
		name, ok := sess[session]
		if !ok {
			continue
		}
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		rss, _ := strconv.ParseUint(f[21], 10, 64)
		c := out.Containers[name]
		c.CPUNs += (ut + st) * tick
		c.MemBytes += rss * page
		out.Containers[name] = c
		_ = pid
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail uint64
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				total = v * 1024
			case "MemAvailable:":
				avail = v * 1024
			}
		}
		out.VMMemBytes = total - avail
	}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		var busy uint64
		for i, v := range f[1:] {
			if i == 3 || i == 4 { // idle, iowait
				continue
			}
			n, _ := strconv.ParseUint(v, 10, 64)
			busy += n
		}
		out.VMCPUNs = busy * tick
	}
	j, _ := json.Marshal(out)
	return j
}
