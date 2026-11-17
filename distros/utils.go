package distros

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var rhelReleases = []string{
	"/etc/rocky-release",
	"/etc/almalinux-release",
	"/etc/redhat-release",
}

var osReleaseFile = "/etc/os-release"

// UseSudo makes RunCommand run the package manager through "sudo -n", for an
// exporter running as an unprivileged user.
var UseSudo bool

func sudoWrap(name string, args []string) (string, []string) {
	if !UseSudo {
		return name, args
	}
	return "sudo", append([]string{"-n", name}, args...)
}

// RunCommand runs a command with extra environment variables and returns its
// standard output and standard error. It is a variable so tests can replace it.
var RunCommand = func(env []string, name string, args ...string) (stdout, stderr []byte, err error) {
	name, args = sudoWrap(name, args)
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

func ParseUpdateCount(out string) int {
	count, _ := strconv.Atoi(strings.TrimSpace(out))
	return count
}

func GetLinuxDistro() string {
	if runtime.GOOS != "linux" {
		return "unknown"
	}

	for _, releaseFile := range rhelReleases {
		if _, err := os.Stat(releaseFile); err == nil {
			return "rhel"
		}
	}

	out, err := exec.Command("sh", "-c", fmt.Sprintf("cat %s", osReleaseFile)).Output()
	if err != nil {
		return "error"
	}

	s := strings.ToLower(string(out))
	switch {
	case strings.Contains(s, "ubuntu"):
		return "ubuntu"
	case strings.Contains(s, "debian"):
		return "debian"
	case strings.Contains(s, "suse"), strings.Contains(s, "sles"):
		return "suse"
	case strings.Contains(s, "rhel"), strings.Contains(s, "centos"), strings.Contains(s, "fedora"):
		return "rhel"
	default:
		return "unknown"
	}
}

func GetLatestChangeInDir(cacheDir string, filterSuffix string) (time.Time, error) {
	var latestTime time.Time
	slog.Debug("GetLatestChangeInDir: Checking Directory", slog.String("directory", cacheDir), slog.String("filterSuffix", filterSuffix))
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return time.Time{}, fmt.Errorf("Failed to read directory %s: %w", cacheDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		if filterSuffix != "" && !strings.HasSuffix(name, filterSuffix) {
			slog.Debug("GetLatestChangeInDir: File does not match filterSuffix, skipping", slog.String("file", name), slog.String("filterSuffix", filterSuffix))
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return time.Time{}, fmt.Errorf("Unable to stat file %s: %w", info.Name(), err)
		}

		modTime := info.ModTime()
		if modTime.After(latestTime) {
			slog.Debug("GetLatestChangeInDir: Found newer modified file", slog.String("file", name), slog.Any("old_time", latestTime), slog.Any("new_time", modTime))
			latestTime = modTime
		}
	}

	return latestTime, nil
}

func DirExists(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("failed to stat dir", slog.String("dir", dir), slog.Any("error", err))
		}
		return false
	}

	if !info.IsDir() {
		slog.Warn("provided dir exists but not a directory", slog.String("file", dir))
		return false
	}

	return true
}
