package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/etcdutl/v3/snapshot"
	"go.uber.org/zap"
	"k8s.io/klog/v2"
)

const launchLabelPrefix = "dev.kvz."

func plistPath(mode string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchLabelPrefix+mode+".plist")
}

// installCmd registers kvz as a launchd user agent (the macOS equivalent of
// the k3s systemd service). It runs in the user's GUI session so
// Virtualization.framework and Local Network access work.
func installCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "install (server|agent) [kvz flags...]",
		Short:              "Install kvz as a launchd service (starts now and at login, restarted on exit)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || (args[0] != "server" && args[0] != "agent") {
				return fmt.Errorf("usage: kvz install server|agent [flags]")
			}
			mode := args[0]
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			home, _ := os.UserHomeDir()
			logDir := filepath.Join(home, ".kvz", "logs")
			_ = os.MkdirAll(logDir, 0o755)
			var argsXML strings.Builder
			for _, a := range append([]string{exe, mode}, args[1:]...) {
				argsXML.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
			}
			plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>2</integer>
  <key>ProcessType</key><string>Interactive</string>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchLabelPrefix, mode, argsXML.String(), filepath.Join(logDir, "kvz-"+mode+".log"), filepath.Join(logDir, "kvz-"+mode+".log"))
			p := plistPath(mode)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			uid := strconv.Itoa(os.Getuid())
			_ = exec.Command("launchctl", "bootout", "gui/"+uid, p).Run() // replace an older install
			if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
				return err
			}
			if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, p).CombinedOutput(); err != nil {
				return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
			}
			fmt.Printf("installed %s (logs: %s)\n", p, filepath.Join(logDir, "kvz-"+mode+".log"))
			if mode == "server" {
				dd := filepath.Join(home, ".kvz")
				for i, a := range args {
					if a == "--data-dir" && i+1 < len(args) {
						dd = args[i+1]
					} else if v, ok := strings.CutPrefix(a, "--data-dir="); ok {
						dd = v
					}
				}
				fmt.Printf("kubeconfig: %s\n", filepath.Join(dd, "kubeconfig.yaml"))
			}
			return nil
		},
	}
}

func uninstallCmd() *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the kvz launchd services (--purge also deletes cluster data)",
		RunE: func(cmd *cobra.Command, args []string) error {
			uid := strconv.Itoa(os.Getuid())
			for _, mode := range []string{"server", "agent"} {
				p := plistPath(mode)
				if _, err := os.Stat(p); err == nil {
					_ = exec.Command("launchctl", "bootout", "gui/"+uid, p).Run()
					_ = os.Remove(p)
					fmt.Println("removed", p)
				}
			}
			if purge {
				home, _ := os.UserHomeDir()
				for _, d := range []string{filepath.Join(home, ".kvz"), "/Users/Shared/kvz-storage"} {
					_ = os.RemoveAll(d)
					fmt.Println("deleted", d)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete ~/.kvz (cluster state, images, kernel) and local-path volumes")
	return c
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// ---- etcd snapshots --------------------------------------------------------------

func snapshotDir(dataDir string) string { return filepath.Join(dataDir, "server", "db", "snapshots") }

// saveSnapshot streams a consistent snapshot from the running embedded etcd.
func saveSnapshot(ctx context.Context, etcdPort int, dir, name string) (string, error) {
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{fmt.Sprintf("http://127.0.0.1:%d", etcdPort)}, DialTimeout: 5 * time.Second, Logger: zap.NewNop()})
	if err != nil {
		return "", err
	}
	defer cli.Close()
	rc, err := cli.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-%s.db", name, time.Now().UTC().Format("20060102T150405Z")))
	f, err := os.OpenFile(p+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	return p, os.Rename(p+".part", p)
}

func pruneSnapshots(dir string, keep int) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	sort.Strings(files)
	for len(files) > keep {
		_ = os.Remove(files[0])
		files = files[1:]
	}
}

// restoreSnapshot replaces the etcd data dir with the snapshot's contents.
func restoreSnapshot(snapPath, etcdDir string, etcdPort int) error {
	if _, err := os.Stat(etcdDir); err == nil {
		bak := etcdDir + ".bak-" + time.Now().Format("20060102T150405")
		if err := os.Rename(etcdDir, bak); err != nil {
			return err
		}
		klog.InfoS("moved previous etcd data aside", "path", bak)
	}
	peer := fmt.Sprintf("http://127.0.0.1:%d", etcdPort+1)
	return snapshot.NewV3(zap.NewNop()).Restore(snapshot.RestoreConfig{
		SnapshotPath: snapPath, Name: "kvz", OutputDataDir: etcdDir,
		PeerURLs: []string{peer}, InitialCluster: "kvz=" + peer, InitialClusterToken: "etcd-cluster", SkipHashCheck: true,
	})
}

func snapshotCmd() *cobra.Command {
	home, _ := os.UserHomeDir()
	var dataDir, name string
	var etcdPort int
	c := &cobra.Command{Use: "etcd-snapshot", Short: "Save, list and delete etcd snapshots"}
	c.PersistentFlags().StringVar(&dataDir, "data-dir", filepath.Join(home, ".kvz"), "kvz data directory")
	c.PersistentFlags().IntVar(&etcdPort, "etcd-port", 2379, "embedded etcd client port")
	save := &cobra.Command{Use: "save", Short: "Take a snapshot of the running server's datastore", RunE: func(cmd *cobra.Command, _ []string) error {
		p, err := saveSnapshot(cmd.Context(), etcdPort, snapshotDir(dataDir), name)
		if err != nil {
			return err
		}
		st, _ := os.Stat(p)
		fmt.Printf("saved %s (%d bytes)\n", p, st.Size())
		return nil
	}}
	save.Flags().StringVar(&name, "name", "on-demand", "snapshot name prefix")
	ls := &cobra.Command{Use: "ls", Short: "List snapshots", RunE: func(cmd *cobra.Command, _ []string) error {
		files, _ := filepath.Glob(filepath.Join(snapshotDir(dataDir), "*.db"))
		sort.Strings(files)
		for _, f := range files {
			st, _ := os.Stat(f)
			fmt.Printf("%-70s %10d  %s\n", f, st.Size(), st.ModTime().Format(time.RFC3339))
		}
		return nil
	}}
	del := &cobra.Command{Use: "delete <file>...", Short: "Delete snapshots", RunE: func(cmd *cobra.Command, args []string) error {
		for _, a := range args {
			if !strings.HasPrefix(filepath.Clean(a), snapshotDir(dataDir)) {
				a = filepath.Join(snapshotDir(dataDir), filepath.Base(a))
			}
			if err := os.Remove(a); err != nil {
				return err
			}
			fmt.Println("deleted", a)
		}
		return nil
	}}
	c.AddCommand(save, ls, del)
	return c
}
