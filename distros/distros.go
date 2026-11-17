package distros

import "time"

type Distro interface {
	GetSecurityUpdates() int
	GetTotalUpdates() int
	GetRebootRequired() bool
	GetLatestCacheChange() time.Time
}

// PatchKey identifies a group of pending patches (advisories).
type PatchKey struct {
	Category string // e.g. security, recommended, optional, feature
	Severity string // e.g. critical, important, moderate, low, unspecified
}

// PatchLister is an optional interface for distros whose package manager
// classifies updates into patches with a category and a severity (zypper).
type PatchLister interface {
	// GetPendingPatches returns the number of needed patches per
	// category/severity. Well-known combinations are returned with a zero
	// count so the resulting time series always exist.
	GetPendingPatches() (map[PatchKey]int, error)
}
