package ubuntu

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	utils "github.com/Puppet-Finland/updates-exporter/distros"
)

var ubuntu = Ubuntu{}

func fakeApt(t *testing.T, file string, runErr error) {
	t.Helper()
	fakeAptStderr(t, file, "", runErr)
}

func fakeAptStderr(t *testing.T, file, stderr string, runErr error) {
	t.Helper()
	old := utils.RunCommand
	utils.RunCommand = func(env []string, name string, args ...string) ([]byte, []byte, error) {
		if name != "apt-get" {
			t.Fatalf("unexpected command %s", name)
		}
		out, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		return out, []byte(stderr), runErr
	}
	t.Cleanup(func() { utils.RunCommand = old })
}

func TestUbuntuRebootRequired(t *testing.T) {
	// Create a fake reboot-required file
	tmpfile := filepath.Join(t.TempDir(), "reboot-required")
	err := os.WriteFile(tmpfile, []byte("reboot"), 0644)
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	oldFile := rebootFile
	rebootFile = tmpfile
	defer func() {
		rebootFile = oldFile
	}()

	if !ubuntu.GetRebootRequired() {
		t.Errorf("Expected reboot required = true")
	}
}

func TestUpdates(t *testing.T) {
	for _, tc := range []struct {
		file            string
		total, security int
	}{
		// Real output of an outdated Debian 12.0.
		{"dist-upgrade-debian-12.txt", 47, 9},
		// Ubuntu origins; "libsecurity-tools" is not a security update.
		{"dist-upgrade-ubuntu.txt", 5, 3},
	} {
		fakeApt(t, tc.file, nil)
		if got := ubuntu.GetTotalUpdates(); got != tc.total {
			t.Errorf("%s: expected %d updates, got %d", tc.file, tc.total, got)
		}
		if got := ubuntu.GetSecurityUpdates(); got != tc.security {
			t.Errorf("%s: expected %d security updates, got %d", tc.file, tc.security, got)
		}
	}
}

func TestAptFailure(t *testing.T) {
	fakeApt(t, "dist-upgrade-ubuntu.txt", errors.New("exit status 100"))
	if got := ubuntu.GetTotalUpdates(); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}
	if got := ubuntu.GetSecurityUpdates(); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}
}

func TestLatestCacheChange(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "update-success-stamp")
	oldStamps, oldDir := updateStamps, cacheDir
	updateStamps, cacheDir = []string{stamp}, dir
	t.Cleanup(func() { updateStamps, cacheDir = oldStamps, oldDir })

	if !ubuntu.GetLatestCacheChange().IsZero() {
		t.Error("expected zero time for an empty lists directory")
	}
	if err := os.WriteFile(stamp, nil, 0644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(stamp)
	if got := ubuntu.GetLatestCacheChange(); !got.Equal(info.ModTime()) {
		t.Errorf("expected %v, got %v", info.ModTime(), got)
	}
}

func TestUnreadableConfiguration(t *testing.T) {
	// apt-get exits 0 but ignores the pinning it cannot read.
	fakeAptStderr(t, "dist-upgrade-ubuntu.txt",
		"W: Unable to read /etc/apt/preferences.d/trixie - open (13: Permission denied)\n", nil)
	if got := ubuntu.GetTotalUpdates(); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}

	// Other warnings are only logged.
	fakeAptStderr(t, "dist-upgrade-ubuntu.txt",
		"W: Target Packages (main/binary-amd64/Packages) is configured multiple times\n", nil)
	if got := ubuntu.GetTotalUpdates(); got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}
