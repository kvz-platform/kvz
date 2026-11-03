package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"golang.org/x/term"
)

// termOut is where kvz's own status lines go (the ready banner, errors). It is
// stderr until quietLogs moves the component logs away from the terminal.
var termOut = os.Stderr

type termWriter struct{}

func (termWriter) Write(p []byte) (int, error) { return termOut.Write(p) }

// quietLogs keeps an interactive terminal readable: when stderr is a terminal
// and --verbose isn't set, everything the embedded components log (kube-apiserver,
// controllers, scheduler, etcd, the node) is sent to <dataDir>/kvz.log instead,
// and only kvz's own status lines reach the terminal. Services and pipes (where
// stderr is a file) keep the full log stream on stderr as before. Returns the
// log path, or "" when logs stay on stderr.
func quietLogs(dataDir string, verbose bool) string {
	if verbose || !term.IsTerminal(int(os.Stderr.Fd())) {
		return ""
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return ""
	}
	path := filepath.Join(dataDir, "kvz.log")
	_ = os.Rename(path, path+".1") // keep the previous run's log
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return ""
	}
	saved, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		f.Close()
		return ""
	}
	// Point fd 2 at the log: klog, etcd's zap logger and the standard library
	// logger all write there, without each needing its own configuration.
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		syscall.Close(saved)
		f.Close()
		return ""
	}
	termOut = os.NewFile(uintptr(saved), "/dev/stderr")
	// A crash still shows on the terminal, not only in the log.
	_ = debug.SetCrashOutput(termOut, debug.CrashOptions{})
	return path
}

func logsHint(path string) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf("  logs: %s (--verbose to stream them here)\n", path)
}
