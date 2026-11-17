package ubuntu

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	utils "github.com/Puppet-Finland/updates-exporter/distros"
)

// Ubuntu handles apt-based distros (Ubuntu, Debian and derivatives).
type Ubuntu struct{}

var (
	rebootFile = "/var/run/reboot-required"
	cacheDir   = "/var/lib/apt/lists"
	// Touched on every successful "apt-get update", in order of preference.
	// The lists themselves only change when the repositories do.
	updateStamps = []string{
		"/var/lib/apt/periodic/update-success-stamp",
		"/var/lib/apt/periodic/update-stamp",
		"/var/lib/apt/lists/partial",
	}

	// Matches the origin of a simulated "Inst" line when the candidate comes
	// from a security archive, e.g.:
	//   Debian-Security:12/stable-security
	//   Ubuntu:22.04/jammy-security
	//   UbuntuESMApps:22.04/jammy-apps-security
	securityOrigin = regexp.MustCompile(`Debian-Security:|/[^ ,/]*-security(?:[ ,]|$)`)
)

func (Ubuntu) GetLatestCacheChange() time.Time {
	for _, stamp := range updateStamps {
		if info, err := os.Stat(stamp); err == nil {
			return info.ModTime()
		}
	}
	change, err := utils.GetLatestChangeInDir(cacheDir, "")
	if err != nil {
		slog.Error("Failed to get latest cache change", slog.Any("error", err))
		return time.Time{}
	}
	return change
}

// parseSimulation counts the packages "apt-get -s" would install, and how
// many of them come from a security archive.
func parseSimulation(out []byte) (total, security int) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		total++
		// Inst <pkg> [<installed>] (<candidate> <origin>[, <origin>...] [<arch>])
		if i := strings.Index(line, "("); i >= 0 && securityOrigin.MatchString(line[i:]) {
			security++
		}
	}
	return total, security
}

// checkStderr returns an error when apt could not read part of its
// configuration. apt only warns about it and goes on, e.g. without the pinning
// of an unreadable /etc/apt/preferences.d file, so the simulated upgrade would
// be wrong. This typically happens when the exporter runs unprivileged.
func checkStderr(stderr []byte) error {
	var problems []string
	scanner := bufio.NewScanner(bytes.NewReader(stderr))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "E:"),
			strings.HasPrefix(line, "W:") && (strings.Contains(line, "Unable to read") || strings.Contains(line, "Permission denied")):
			problems = append(problems, line)
		case strings.HasPrefix(line, "W:"):
			slog.Warn("apt-get warning", slog.String("message", line))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("apt-get could not read its whole configuration, make these files readable by the exporter's user or use --sudo: %s",
			strings.Join(problems, "; "))
	}
	return nil
}

func simulateUpgrade() (total, security int, err error) {
	out, stderr, err := utils.RunCommand([]string{"LC_ALL=C"}, "apt-get", "-s", "-o", "Debug::NoLocking=true", "dist-upgrade")
	if err == nil {
		err = checkStderr(stderr)
	} else if len(stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr))
	}
	if err != nil {
		slog.Error("Failed running apt-get", slog.Any("error", err))
		return 0, 0, err
	}
	total, security = parseSimulation(out)
	return total, security, nil
}

func (Ubuntu) GetSecurityUpdates() int {
	_, security, err := simulateUpgrade()
	if err != nil {
		return -1
	}
	return security
}

func (Ubuntu) GetTotalUpdates() int {
	total, _, err := simulateUpgrade()
	if err != nil {
		return -1
	}
	return total
}

func (Ubuntu) GetRebootRequired() bool {
	if _, err := os.Stat(rebootFile); err == nil {
		return true
	}
	return false
}
