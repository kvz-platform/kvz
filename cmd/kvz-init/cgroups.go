//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Per-container cgroup v2 limits, as the kubelet and containerd apply them:
// cpu.max from limits.cpu, cpu.weight from requests.cpu, memory.max from
// limits.memory. Each container sees its own cgroup at /sys/fs/cgroup.

// Cgroup holds a container's cgroup settings ("" leaves the default).
type Cgroup struct {
	CPUMax    string `json:"cpu_max,omitempty"`    // "<quota> <period>" or "max <period>"
	CPUWeight int    `json:"cpu_weight,omitempty"` // 1..10000
	MemoryMax string `json:"memory_max,omitempty"` // bytes or "max"
}

const cgRoot = "/sys/fs/cgroup"

var cgroupsOn bool

// oomSeen is the last oom_kill count observed per container (reaper only).
var oomSeen = map[string]int{}

// setupCgroups mounts cgroup v2 and enables the controllers for children.
func setupCgroups() {
	_ = os.MkdirAll("/sys", 0o755)
	_ = unix.Mount("sysfs", "/sys", "sysfs", 0, "") // EBUSY if already mounted
	if err := unix.Mount("cgroup2", cgRoot, "cgroup2", 0, ""); err != nil && err != unix.EBUSY {
		return // kernel without cgroups: no per-container limits
	}
	ctrls, _ := os.ReadFile(cgRoot + "/cgroup.controllers")
	var enable []string
	for _, c := range strings.Fields(string(ctrls)) {
		if c == "cpu" || c == "memory" || c == "pids" || c == "cpuset" {
			enable = append(enable, "+"+c)
		}
	}
	if len(enable) == 0 {
		return
	}
	_ = os.WriteFile(cgRoot+"/cgroup.subtree_control", []byte(strings.Join(enable, " ")), 0o644)
	cgroupsOn = true
}

func cgPath(container string) string { return cgRoot + "/" + container }

// applyCgroup creates (or updates) a container's cgroup.
func applyCgroup(container string, g Cgroup) {
	if !cgroupsOn {
		return
	}
	p := cgPath(container)
	_ = os.MkdirAll(p, 0o755)
	if g.CPUMax != "" {
		_ = os.WriteFile(p+"/cpu.max", []byte(g.CPUMax), 0o644)
	}
	if g.CPUWeight > 0 {
		_ = os.WriteFile(p+"/cpu.weight", []byte(strconv.Itoa(g.CPUWeight)), 0o644)
	}
	if g.MemoryMax != "" {
		_ = os.WriteFile(p+"/memory.max", []byte(g.MemoryMax), 0o644)
		_ = os.WriteFile(p+"/memory.swap.max", []byte("0"), 0o644)
	}
}

// oomKills reads a container cgroup's oom_kill counter.
func oomKills(container string) int {
	b, err := os.ReadFile(cgPath(container) + "/memory.events")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "oom_kill" {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

// mountContainerCgroup shows the container its own cgroup at /sys/fs/cgroup.
func mountContainerCgroup(root, container string) {
	if !cgroupsOn {
		return
	}
	_ = os.MkdirAll(cgPath(container), 0o755)
	dst := secureJoin(root, cgRoot)
	if err := unix.Mount(cgPath(container), dst, "", unix.MS_BIND, ""); err == nil {
		_ = unix.Mount("", dst, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, "")
	}
}
