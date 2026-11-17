package suse

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	utils "github.com/Puppet-Finland/updates-exporter/distros"
)

// fakeZypper makes RunCommand return the content of a testdata file per
// zypper subcommand, and counts the calls.
func fakeZypper(t *testing.T, files map[string]string, runErr error) *int {
	t.Helper()
	calls := 0
	old := utils.RunCommand
	utils.RunCommand = func(env []string, name string, args ...string) ([]byte, []byte, error) {
		calls++
		if name != "zypper" {
			t.Fatalf("unexpected command %s", name)
		}
		for _, arg := range args {
			if file, ok := files[arg]; ok {
				out, err := os.ReadFile(filepath.Join("testdata", file))
				if err != nil {
					t.Fatal(err)
				}
				return out, nil, runErr
			}
		}
		t.Fatalf("unexpected zypper arguments %v", args)
		return nil, nil, nil
	}
	t.Cleanup(func() { utils.RunCommand = old })
	return &calls
}

// Real output of zypper 1.14.76 on an outdated openSUSE Leap 15.5.
var leap = map[string]string{
	"list-patches": "list-patches-leap-15.5.xml",
	"list-updates": "list-updates-package-leap-15.5.xml",
}

func TestPendingPatches(t *testing.T) {
	calls := fakeZypper(t, leap, nil)
	s := &Suse{}

	patches, err := s.GetPendingPatches()
	if err != nil {
		t.Fatal(err)
	}
	// 4 patches for the package manager itself plus 14 blocked by them.
	expected := map[utils.PatchKey]int{
		{Category: "security", Severity: "important"}:    1,
		{Category: "security", Severity: "moderate"}:     3,
		{Category: "recommended", Severity: "important"}: 2,
		{Category: "recommended", Severity: "moderate"}:  12,
	}
	total := 0
	for key, count := range patches {
		total += count
		if count != expected[key] {
			t.Errorf("%+v: expected %d, got %d", key, expected[key], count)
		}
	}
	if total != 18 {
		t.Errorf("expected 18 patches, got %d", total)
	}
	// Well-known combinations exist even without any pending patch.
	if _, ok := patches[utils.PatchKey{Category: "optional", Severity: "low"}]; !ok {
		t.Error("expected a zero count for optional/low")
	}

	if got := s.GetSecurityUpdates(); got != 4 {
		t.Errorf("expected 4 security patches, got %d", got)
	}
	if *calls != 1 {
		t.Errorf("expected list-patches to run once per cycle, ran %d times", *calls)
	}
}

func TestTotalUpdates(t *testing.T) {
	fakeZypper(t, leap, nil)
	if got := (&Suse{}).GetTotalUpdates(); got != 22 {
		t.Errorf("expected 22 package updates, got %d", got)
	}
}

func TestNoUpdates(t *testing.T) {
	fakeZypper(t, map[string]string{"list-patches": "none.xml", "list-updates": "none.xml"}, nil)
	s := &Suse{}
	if got := s.GetSecurityUpdates(); got != 0 {
		t.Errorf("expected 0 security patches, got %d", got)
	}
	if got := s.GetTotalUpdates(); got != 0 {
		t.Errorf("expected 0 package updates, got %d", got)
	}
}

func TestZypperFailure(t *testing.T) {
	fakeZypper(t, map[string]string{"list-patches": "locked.xml", "list-updates": "locked.xml"}, errors.New("exit status 7"))
	s := &Suse{}
	if _, err := s.GetPendingPatches(); err == nil {
		t.Error("expected an error when zypper is locked")
	}
	if got := s.GetSecurityUpdates(); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}
	if got := s.GetTotalUpdates(); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}
}

func TestRebootAndCacheChange(t *testing.T) {
	dir := t.TempDir()
	oldReboot, oldGlobs := rebootFile, cookieGlobs
	rebootFile = filepath.Join(dir, "reboot-needed")
	cookieGlobs = []string{filepath.Join(dir, "raw", "*", "repodata", "repomd.xml")}
	t.Cleanup(func() { rebootFile, cookieGlobs = oldReboot, oldGlobs })

	s := &Suse{}
	if s.GetRebootRequired() {
		t.Error("expected reboot required = false")
	}
	if err := os.WriteFile(rebootFile, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if !s.GetRebootRequired() {
		t.Error("expected reboot required = true")
	}

	if !s.GetLatestCacheChange().IsZero() {
		t.Error("expected zero time without any metadata")
	}
	newest := time.Now().Add(-time.Hour).Truncate(time.Second)
	for repo, mtime := range map[string]time.Time{"repo-oss": newest.Add(-time.Hour), "repo-update": newest} {
		cookie := filepath.Join(dir, "raw", repo, "repodata", "repomd.xml")
		if err := os.MkdirAll(filepath.Dir(cookie), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cookie, nil, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(cookie, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.GetLatestCacheChange(); !got.Equal(newest) {
		t.Errorf("expected %v, got %v", newest, got)
	}
}

func TestSkippedRepositories(t *testing.T) {
	// Exit code 106: valid output, but from a subset of the repositories.
	fakeZypper(t, leap, errors.New("exit status 106"))
	if got := (&Suse{}).GetSecurityUpdates(); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}
}
