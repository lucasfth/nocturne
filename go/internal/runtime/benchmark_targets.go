// SPDX-License-Identifier: AGPL-3.0-or-later
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"golang.org/x/sys/unix"
)

const benchmarkTargetsLimit = 65536

var errBenchmarkTargets = errors.New("invalid or unprotected benchmark target installation attestation")

type benchmarkInstallation struct {
	CapabilityVersion    int    `json:"capability_version"`
	ManifestSHA256       string `json:"manifest_sha256"`
	ReviewedInstallation bool   `json:"reviewed_installation"`
	ServiceControl       bool   `json:"service_control"`
	BenchmarkExecution   bool   `json:"benchmark_execution"`
	IndependentWatchdog  bool   `json:"independent_watchdog"`
	DurableRestoration   bool   `json:"durable_restoration"`
	AdmissionFencing     bool   `json:"admission_fencing"`
	ArtifactVerification bool   `json:"artifact_verification"`
}
type productionApplyInstallation struct {
	Protocol              int    `json:"protocol"`
	ManifestSHA256        string `json:"manifest_sha256"`
	DurableDecisions      bool   `json:"durable_decisions"`
	ConfigurationRollback bool   `json:"configuration_rollback"`
	ReviewedInstallation  bool   `json:"reviewed_installation"`
}
type benchmarkTargetEntry struct {
	ProductionApply  *productionApplyInstallation `json:"production_apply,omitempty"`
	Machine          string                       `json:"machine"`
	ModelSHA256      string                       `json:"model_sha256"`
	BuildDigest      string                       `json:"build_digest"`
	CodeRevision     string                       `json:"code_revision"`
	ConfigGeneration string                       `json:"config_generation"`
	Installation     benchmarkInstallation        `json:"installation"`
}
type benchmarkTargetFile struct {
	Version int                    `json:"version"`
	Targets []benchmarkTargetEntry `json:"targets"`
}

// Reject duplicate, case-aliased and unknown keys before typed decoding.
func benchmarkJSONKeys(d *json.Decoder, depth int) error {
	if depth > 6 {
		return errBenchmarkTargets
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		allowed := " version targets machine model_sha256 build_digest code_revision config_generation installation capability_version manifest_sha256 reviewed_installation service_control benchmark_execution independent_watchdog durable_restoration admission_fencing artifact_verification production_apply protocol durable_decisions configuration_rollback "
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] || !strings.Contains(allowed, " "+s+" ") {
				return errBenchmarkTargets
			}
			seen[s] = true
			if e := benchmarkJSONKeys(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := benchmarkJSONKeys(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return errBenchmarkTargets
	}
	_, e = d.Token()
	return e
}

func parseBenchmarkTargets(raw []byte, enabled func(string) bool) (map[string]app.BenchmarkTarget, error) {
	if len(raw) > benchmarkTargetsLimit {
		return nil, errBenchmarkTargets
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if benchmarkJSONKeys(d, 0) != nil {
		return nil, errBenchmarkTargets
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, errBenchmarkTargets
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var config benchmarkTargetFile
	if d.Decode(&config) != nil || config.Version != 1 || len(config.Targets) == 0 || len(config.Targets) > 128 {
		return nil, errBenchmarkTargets
	}
	hash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	rev := regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	result := map[string]app.BenchmarkTarget{}
	for _, v := range config.Targets {
		a := v.Installation
		if !machines.ValidID(v.Machine) || !enabled(v.Machine) || !hash.MatchString(v.ModelSHA256) || !hash.MatchString(v.BuildDigest) || !rev.MatchString(v.CodeRevision) || !hash.MatchString(v.ConfigGeneration) || a.CapabilityVersion != 1 || !hash.MatchString(a.ManifestSHA256) || !a.ReviewedInstallation || !a.ServiceControl || !a.BenchmarkExecution || !a.IndependentWatchdog || !a.DurableRestoration || !a.AdmissionFencing || !a.ArtifactVerification {
			return nil, errBenchmarkTargets
		}
		if _, ok := result[v.Machine]; ok {
			return nil, errBenchmarkTargets
		}
		apply := v.ProductionApply
		if apply != nil && (apply.Protocol != 1 || !hash.MatchString(apply.ManifestSHA256) || !apply.DurableDecisions || !apply.ConfigurationRollback || !apply.ReviewedInstallation) {
			return nil, errBenchmarkTargets
		}
		result[v.Machine] = app.BenchmarkTarget{ModelSHA256: v.ModelSHA256, BuildDigest: v.BuildDigest, CodeRevision: v.CodeRevision, ConfigGeneration: v.ConfigGeneration, ApplyEnabled: apply != nil}
	}
	return result, nil
}

// Open every component relative to its pinned parent FD, never following links.
// All ancestors must be root owned and not group/other writable (even sticky /tmp).
// On Darwin, every opened descriptor must also have no extended ACL.
func protectedBenchmarkTargets(root string) ([]byte, error) {
	path, e := filepath.Abs(root)
	if e != nil {
		return nil, errBenchmarkTargets
	}
	// Abs cleans paths: reject explicit traversal before that normalization.
	for _, p := range strings.Split(root, string(os.PathSeparator)) {
		if p == ".." {
			return nil, errBenchmarkTargets
		}
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, errBenchmarkTargets
	}
	defer func() { unix.Close(fd) }()
	check := func(fd int, dir bool) bool {
		var st unix.Stat_t
		if !benchmarkDescriptorNoACL(fd) || unix.Fstat(fd, &st) != nil || st.Uid != 0 || st.Mode&0022 != 0 {
			return false
		}
		if dir {
			return st.Mode&unix.S_IFMT == unix.S_IFDIR
		}
		return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&0777 == 0600 && st.Nlink == 1 && st.Size <= benchmarkTargetsLimit
	}
	if !check(fd, true) {
		return nil, errBenchmarkTargets
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if e != nil {
			return nil, errBenchmarkTargets
		}
		unix.Close(fd)
		fd = next
		if !check(fd, true) {
			return nil, errBenchmarkTargets
		}
	}
	file, e := unix.Openat(fd, "benchmark-targets.json", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(e, unix.ENOENT) {
		return nil, nil
	}
	if e != nil {
		return nil, errBenchmarkTargets
	}
	f := os.NewFile(uintptr(file), "benchmark-targets.json")
	defer f.Close()
	if !check(file, false) {
		return nil, errBenchmarkTargets
	}
	raw, e := io.ReadAll(io.LimitReader(f, benchmarkTargetsLimit+1))
	if e != nil || len(raw) > benchmarkTargetsLimit {
		return nil, errBenchmarkTargets
	}
	return raw, nil
}

func loadBenchmarkTargets(root string, registry *machines.Machines) (map[string]app.BenchmarkTarget, error) {
	// Absence preserves unprivileged default startup. This probe grants no trust:
	// any present entry must subsequently pass the descriptor-only protected walk.
	_, e := os.Lstat(filepath.Join(root, "benchmark-targets.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, errBenchmarkTargets
	}
	raw, e := protectedBenchmarkTargets(root)
	if e != nil || raw == nil {
		return nil, errBenchmarkTargets
	}
	return parseBenchmarkTargets(raw, registry.Enabled)
}
