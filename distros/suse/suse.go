// Package suse handles zypper-based distros (openSUSE Leap/Tumbleweed, SLES).
package suse

import (
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	utils "github.com/Puppet-Finland/updates-exporter/distros"
)

var (
	// Created by libzypp when an installed update requires a reboot; this is
	// what "zypper needs-rebooting" checks.
	rebootFile = "/run/reboot-needed"
	// libzypp touches the cached repomd.xml each time it checks a repository,
	// even when the metadata did not change. The solv cookie, used as a
	// fallback, only changes along with the metadata.
	cookieGlobs = []string{
		"/var/cache/zypp/raw/*/repodata/repomd.xml",
		"/var/cache/zypp/solv/*/cookie",
	}

	// Always exported, even when no such patch is pending.
	knownCategories = []string{"security", "recommended", "optional", "feature"}
	knownSeverities = []string{"critical", "important", "moderate", "low", "unspecified"}

	// The patch list is needed by several getters of the same refresh cycle.
	patchCacheTTL = time.Minute
)

type update struct {
	Kind     string `xml:"kind,attr"`
	Name     string `xml:"name,attr"`
	Category string `xml:"category,attr"`
	Severity string `xml:"severity,attr"`
}

// stream is the subset of "zypper --xmlout list-updates|list-patches" we need.
type stream struct {
	Status *struct {
		Updates []update `xml:"update-list>update"`
		// Patches that can only be applied once the patches affecting the
		// package manager itself (in update-list) are installed.
		Blocked []update `xml:"blocked-update-list>update"`
	} `xml:"update-status"`
	Messages []struct {
		Type string `xml:"type,attr"`
		Text string `xml:",chardata"`
	} `xml:"message"`
}

type Suse struct {
	mu        sync.Mutex
	patches   []update
	patchesAt time.Time
}

// parseUpdates returns the updates of the given kind found in zypper's XML
// output, including the blocked ones.
func parseUpdates(out []byte, kind string) ([]update, error) {
	var s stream
	if err := xml.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("parsing zypper output: %w", err)
	}
	if s.Status == nil {
		for _, m := range s.Messages {
			if m.Type == "error" {
				return nil, fmt.Errorf("zypper: %s", m.Text)
			}
		}
		return nil, errors.New("zypper output contains no update-status")
	}
	updates := []update{}
	for _, u := range append(s.Status.Updates, s.Status.Blocked...) {
		if u.Kind == kind {
			updates = append(updates, u)
		}
	}
	return updates, nil
}

func zypperList(kind string, args ...string) ([]update, error) {
	args = append([]string{"--quiet", "--non-interactive", "--xmlout"}, args...)
	out, _, runErr := utils.RunCommand(nil, "zypper", args...)
	updates, err := parseUpdates(out, kind)
	if runErr != nil {
		// Includes exit code 106 (some repositories were skipped): the list
		// would be incomplete, and reporting too few updates is worse than
		// reporting a failure.
		if err != nil {
			return nil, fmt.Errorf("%w (%v)", err, runErr)
		}
		return nil, fmt.Errorf("zypper %v: %w", args, runErr)
	}
	return updates, err
}

func (s *Suse) listPatches() ([]update, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.patches != nil && time.Since(s.patchesAt) < patchCacheTTL {
		return s.patches, nil
	}
	patches, err := zypperList("patch", "list-patches")
	if err != nil {
		return nil, err
	}
	s.patches, s.patchesAt = patches, time.Now()
	return patches, nil
}

func countPatches(patches []update) map[utils.PatchKey]int {
	counts := map[utils.PatchKey]int{}
	for _, c := range knownCategories {
		for _, sev := range knownSeverities {
			counts[utils.PatchKey{Category: c, Severity: sev}] = 0
		}
	}
	for _, p := range patches {
		key := utils.PatchKey{Category: p.Category, Severity: p.Severity}
		if key.Category == "" {
			key.Category = "unspecified"
		}
		if key.Severity == "" {
			key.Severity = "unspecified"
		}
		counts[key]++
	}
	return counts
}

// GetPendingPatches returns the needed patches per category and severity.
func (s *Suse) GetPendingPatches() (map[utils.PatchKey]int, error) {
	patches, err := s.listPatches()
	if err != nil {
		return nil, err
	}
	return countPatches(patches), nil
}

// GetSecurityUpdates returns the number of needed security patches.
func (s *Suse) GetSecurityUpdates() int {
	patches, err := s.listPatches()
	if err != nil {
		slog.Error("Failed listing patches", slog.Any("error", err))
		return -1
	}
	count := 0
	for _, p := range patches {
		if p.Category == "security" {
			count++
		}
	}
	return count
}

// GetTotalUpdates returns the number of installed packages for which a newer
// version is available, whether or not a patch provides it.
func (s *Suse) GetTotalUpdates() int {
	// Not combined with list-patches: with several --type options, zypper
	// only reports patches.
	packages, err := zypperList("package", "list-updates", "--type", "package")
	if err != nil {
		slog.Error("Failed listing package updates", slog.Any("error", err))
		return -1
	}
	return len(packages)
}

func (s *Suse) GetRebootRequired() bool {
	_, err := os.Stat(rebootFile)
	return err == nil
}

func (s *Suse) GetLatestCacheChange() time.Time {
	var latest time.Time
	for _, pattern := range cookieGlobs {
		cookies, _ := filepath.Glob(pattern)
		for _, cookie := range cookies {
			if info, err := os.Stat(cookie); err == nil && info.ModTime().After(latest) {
				latest = info.ModTime()
			}
		}
		if !latest.IsZero() {
			break
		}
	}
	if latest.IsZero() {
		slog.Error("Failed to get latest cache change: no repository metadata found")
	}
	return latest
}
