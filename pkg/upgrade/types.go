// Package upgrade implements automated kvz upgrades, the kvz equivalent of
// the k3s system-upgrade-controller.
//
// Users write the same upgrade.cattle.io/v1 Plan objects they would for k3s.
// kvz can't run the upstream controller's model, an upgrade Job with the host
// mounted that replaces the binary and lets systemd restart it: kvz pods are
// micro-VMs or child processes of the kvz process itself, and nothing in a pod
// can replace or restart the host binary. Instead:
//
//   - the server runs a Plan controller in-process (leader-elected in HA). It
//     resolves the Plan's version or channel, picks nodes (control plane first,
//     honoring concurrency), cordons and drains them through the API, and then
//     asks the node to upgrade by annotating it;
//   - every node runs an agent that watches its own Node object. It downloads
//     the release tarball, verifies it (sha256, code signature, virtualization
//     entitlement, signing team, reported version), swaps the binary and pod
//     kernel atomically, and re-executes itself;
//   - on boot, a binary that was just installed counts its start attempts and
//     rolls back to the previous binary if it never becomes Ready.
package upgrade

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	Group    = "upgrade.cattle.io"
	Version  = "v1"
	Resource = "plans"

	// LabelPrefix + plan name is set on a node once it runs the plan's
	// version, with the plan's latest hash as value (as upstream).
	LabelPrefix = "plan.upgrade.cattle.io/"

	// AnnRelease is the kvz release a node runs, set by the node's agent once
	// the node is Ready on it.
	AnnRelease = "kvz.dev/release"
	// AnnPlan names the plan (namespace/name) upgrading a node.
	AnnPlan = "upgrade.kvz.dev/plan"
	// AnnApply tells a node's agent to install this release now (set after
	// cordon and drain), with the tarball at AnnURL.
	AnnApply = "upgrade.kvz.dev/apply"
	AnnURL   = "upgrade.kvz.dev/url"
	// AnnCordoned marks nodes the controller cordoned, so it only uncordons
	// nodes that were schedulable before.
	AnnCordoned = "upgrade.kvz.dev/cordoned"
	// AnnStatus is the agent's progress report (Downloading, Verifying,
	// Restarting, Failed: ...).
	AnnStatus = "upgrade.kvz.dev/status"

	// Plan annotations (kvz extensions; upstream Plans work without them).
	PlanAnnReleaseURL     = "upgrade.kvz.dev/release-url"
	PlanAnnAllowDowngrade = "upgrade.kvz.dev/allow-downgrade"

	// DefaultReleaseURL is where release tarballs live; {version} is replaced
	// with the resolved release. The checksum is at the same URL + ".sha256".
	DefaultReleaseURL = "https://github.com/kvz-platform/kvz/releases/download/{version}/kvz-{version}-darwin-arm64.tar.gz"

	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
)

// Plan is the subset of the upstream upgrade.cattle.io/v1 Plan that kvz
// acts on. Fields that only make sense for Jobs (upgrade.image, prepare,
// serviceAccountName, tolerations, ...) are accepted and ignored.
type Plan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PlanSpec   `json:"spec"`
	Status            PlanStatus `json:"status,omitempty"`
}

type PlanSpec struct {
	Concurrency  int64                 `json:"concurrency,omitempty"`
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	Version      string                `json:"version,omitempty"`
	Channel      string                `json:"channel,omitempty"`
	Cordon       bool                  `json:"cordon,omitempty"`
	Drain        *DrainSpec            `json:"drain,omitempty"`
}

type DrainSpec struct {
	Timeout            *Duration             `json:"timeout,omitempty"`
	GracePeriod        *int32                `json:"gracePeriod,omitempty"`
	DeleteLocalData    *bool                 `json:"deleteLocalData,omitempty"`
	DeleteEmptydirData *bool                 `json:"deleteEmptydirData,omitempty"`
	IgnoreDaemonSets   *bool                 `json:"ignoreDaemonSets,omitempty"`
	Force              bool                  `json:"force,omitempty"`
	DisableEviction    bool                  `json:"disableEviction,omitempty"`
	SkipWaitForDelete  int                   `json:"skipWaitForDeleteTimeout,omitempty"`
	PodSelector        *metav1.LabelSelector `json:"podSelector,omitempty"`
}

type PlanStatus struct {
	Conditions    []Condition `json:"conditions,omitempty"`
	LatestVersion string      `json:"latestVersion,omitempty"`
	LatestHash    string      `json:"latestHash,omitempty"`
	Applying      []string    `json:"applying,omitempty"`
}

type Condition struct {
	Type           string `json:"type"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	Message        string `json:"message,omitempty"`
	LastUpdateTime string `json:"lastUpdateTime,omitempty"`
}

// Duration accepts a Go duration string ("5m") or, as upstream encodes
// time.Duration, an integer number of nanoseconds.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		d.Duration = time.Duration(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	v, err := time.ParseDuration(str)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// releaseNumber returns the release counter of a kvz version
// (v1.35.10-12 -> 12). Dev builds (v1.35.10-dev+abc) have none.
func releaseNumber(v string) (int, bool) {
	i := strings.LastIndexByte(v, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[i+1:])
	return n, err == nil
}

// isDowngrade reports whether moving from cur to next lowers the release
// number. Versions without a release number are never a downgrade.
func isDowngrade(cur, next string) bool {
	c, ok1 := releaseNumber(cur)
	n, ok2 := releaseNumber(next)
	return ok1 && ok2 && n < c
}

// ReleaseURL expands a release URL template.
func ReleaseURL(tmpl, version string) string {
	if tmpl == "" {
		tmpl = DefaultReleaseURL
	}
	return strings.ReplaceAll(tmpl, "{version}", version)
}

// DeepCopy copies a status (conditions and applying are the only references).
func (s *PlanStatus) DeepCopy() *PlanStatus {
	out := *s
	out.Conditions = append([]Condition(nil), s.Conditions...)
	out.Applying = append([]string(nil), s.Applying...)
	return &out
}
