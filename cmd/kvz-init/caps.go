//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Linux capabilities, as containerd/runc apply them for Kubernetes: privileged
// containers get every capability; others the default set plus
// securityContext.capabilities.add minus drop. Non-root users start with an
// empty effective set (the bounding set still limits setuid/file caps), and
// allowPrivilegeEscalation: false sets no_new_privs.

var capByName = map[string]int{"CHOWN": 0, "DAC_OVERRIDE": 1, "DAC_READ_SEARCH": 2, "FOWNER": 3, "FSETID": 4, "KILL": 5,
	"SETGID": 6, "SETUID": 7, "SETPCAP": 8, "LINUX_IMMUTABLE": 9, "NET_BIND_SERVICE": 10, "NET_BROADCAST": 11,
	"NET_ADMIN": 12, "NET_RAW": 13, "IPC_LOCK": 14, "IPC_OWNER": 15, "SYS_MODULE": 16, "SYS_RAWIO": 17,
	"SYS_CHROOT": 18, "SYS_PTRACE": 19, "SYS_PACCT": 20, "SYS_ADMIN": 21, "SYS_BOOT": 22, "SYS_NICE": 23,
	"SYS_RESOURCE": 24, "SYS_TIME": 25, "SYS_TTY_CONFIG": 26, "MKNOD": 27, "LEASE": 28, "AUDIT_WRITE": 29,
	"AUDIT_CONTROL": 30, "SETFCAP": 31, "MAC_OVERRIDE": 32, "MAC_ADMIN": 33, "SYSLOG": 34, "WAKE_ALARM": 35,
	"BLOCK_SUSPEND": 36, "AUDIT_READ": 37, "PERFMON": 38, "BPF": 39, "CHECKPOINT_RESTORE": 40}

const lastCap = 40

var defaultCaps = []string{"CHOWN", "DAC_OVERRIDE", "FSETID", "FOWNER", "MKNOD", "NET_RAW", "SETGID", "SETUID",
	"SETFCAP", "SETPCAP", "NET_BIND_SERVICE", "SYS_CHROOT", "KILL", "AUDIT_WRITE"}

func capMask(c Container) uint64 {
	const all = (uint64(1) << (lastCap + 1)) - 1
	if c.Privileged {
		return all
	}
	var m uint64
	for _, n := range defaultCaps {
		m |= 1 << capByName[n]
	}
	norm := func(s string) string { return strings.TrimPrefix(strings.ToUpper(s), "CAP_") }
	for _, a := range c.CapAdd {
		if norm(a) == "ALL" {
			m = all
		} else if n, ok := capByName[norm(a)]; ok {
			m |= 1 << n
		}
	}
	for _, d := range c.CapDrop {
		if norm(d) == "ALL" {
			m = 0
		} else if n, ok := capByName[norm(d)]; ok {
			m &^= 1 << n
		}
	}
	return m
}

// launch is what the kvz-exec trampoline needs to become a container process.
type launch struct {
	Root, Dir, Path string
	Argv, Env       []string
	UID, GID        int
	Groups          []uint32
	Caps            uint64
	NoNewPrivs      bool
}

// containerCmd returns a command that starts argv as a process of container
// c (via the trampoline, so the bounding set can be dropped before exec).
func containerCmd(c Container, argv []string) (*exec.Cmd, error) {
	root := "/c/" + c.Name
	path := resolve(root, argv[0], c.Env)
	dir := c.Cwd
	if dir == "" {
		dir = "/"
	}
	check := path
	if !filepath.IsAbs(check) { // relative to the working directory, like execve after chdir
		check = filepath.Join(dir, check)
	}
	if st, err := os.Stat(secureJoin(root, check)); err != nil {
		return nil, fmt.Errorf("exec: %q: no such file or directory", argv[0])
	} else if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("exec: %q: permission denied", argv[0])
	}
	l := launch{Root: root, Dir: dir, Path: path, Argv: argv, Env: c.Env, UID: c.UID, GID: c.GID,
		Groups: groups(c, root), Caps: capMask(c), NoNewPrivs: c.NoNewPrivs && !c.Privileged}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	return &exec.Cmd{Path: "/proc/self/exe", Args: []string{"kvz-exec"}, Env: []string{"KVZ_LAUNCH=" + string(b)},
		SysProcAttr: &syscall.SysProcAttr{}}, nil
}

// trampoline runs in the child: one locked OS thread applies the security
// context and execs the container process.
func trampoline() {
	runtime.LockOSThread()
	var l launch
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "kvz-exec: %s: %v\n", what, err)
		os.Exit(127)
	}
	if err := json.Unmarshal([]byte(os.Getenv("KVZ_LAUNCH")), &l); err != nil {
		fail("launch spec", err)
	}
	if err := unix.Chroot(l.Root); err != nil {
		fail("chroot", err)
	}
	if err := unix.Chdir(l.Dir); err != nil {
		_ = unix.Chdir("/")
	}
	for c := 0; c <= lastCap; c++ {
		if l.Caps&(1<<c) == 0 {
			_ = unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0)
		}
	}
	gs := make([]int, len(l.Groups))
	for i, g := range l.Groups {
		gs[i] = int(g)
	}
	if err := syscall.Setgroups(gs); err != nil {
		fail("setgroups", err)
	}
	if err := syscall.Setgid(l.GID); err != nil {
		fail("setgid", err)
	}
	if err := syscall.Setuid(l.UID); err != nil {
		fail("setuid", err)
	}
	if l.UID == 0 {
		hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		lo, hi := uint32(l.Caps), uint32(l.Caps>>32)
		data := [2]unix.CapUserData{{Effective: lo, Permitted: lo, Inheritable: lo}, {Effective: hi, Permitted: hi, Inheritable: hi}}
		if err := unix.Capset(&hdr, &data[0]); err != nil {
			fail("capset", err)
		}
	}
	if l.NoNewPrivs {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			fail("no_new_privs", err)
		}
	}
	err := syscall.Exec(l.Path, l.Argv, l.Env)
	fail("exec "+l.Path, err)
}
