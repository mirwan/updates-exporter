package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/Puppet-Finland/updates-exporter/distros"
	"github.com/Puppet-Finland/updates-exporter/distros/rhel"
	"github.com/Puppet-Finland/updates-exporter/distros/suse"
	"github.com/Puppet-Finland/updates-exporter/distros/ubuntu"
)

type Config struct {
	Port          int    `mapstructure:"port"`
	ListenAddress string `mapstructure:"listen-address"`
	TelemetryPath string `mapstructure:"telemetry-path"`
	WebConfigFile string `mapstructure:"web-config-file"`
	Interval      int    `mapstructure:"interval"`
	Sudo          bool   `mapstructure:"sudo"`
	Version       bool   `mapstructure:"version"`
	LogLevel      string `mapstructure:"log-level"`
	Config        string `mapstructure:"config"`
}

const (
	DEFAULT_INTERVAL = 3600
	DEFAULT_PORT     = 9101
	// Delay before retrying a failed query, doubled after each failure
	RETRY_INTERVAL = time.Minute
)

var (
	securityUpdates = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "updates_pending_security",
		Help: "Number of pending security updates",
	})
	totalUpdates = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "updates_pending",
		Help: "Total number of pending updates",
	})
	rebootRequired = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "updates_reboot_required",
		Help: "1 if a reboot is required, 0 otherwise",
	})
	latestCacheUpdate = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "updates_latest_cache_change",
		Help: "Unix timestamp of latest change to update cache",
	})
	pendingPatches = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "updates_pending_patches",
		Help: "Number of pending patches by category and severity (zypper only)",
	}, []string{"category", "severity"})
	lastRefresh = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "updates_last_refresh_timestamp_seconds",
		Help: "Unix timestamp of the last time the package manager was queried",
	})
	lastRefreshSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "updates_last_refresh_success",
		Help: "1 if the last query of the package manager succeeded, 0 otherwise",
	})
	Version = "dev"

	// Label sets currently exported by pendingPatches.
	exportedPatches = map[distros.PatchKey]struct{}{}
)

func getDistro() distros.Distro {
	switch distros.GetLinuxDistro() {
	case "ubuntu", "debian":
		return ubuntu.Ubuntu{}
	case "suse":
		return &suse.Suse{}
	case "rhel":
		return rhel.Rhel{}
	default:
		return nil
	}
}

// updateMetrics queries the package manager and returns whether it succeeded.
func updateMetrics(d distros.Distro) bool {
	if d == nil {
		return false
	}
	security, total := d.GetSecurityUpdates(), d.GetTotalUpdates()
	success := security >= 0 && total >= 0
	securityUpdates.Set(float64(security))
	totalUpdates.Set(float64(total))
	latestCacheUpdate.Set(float64(d.GetLatestCacheChange().Unix()))

	if p, ok := d.(distros.PatchLister); ok {
		if err := updatePatches(p); err != nil {
			slog.Error("Failed getting pending patches", slog.Any("error", err))
			success = false
		}
	}

	lastRefresh.SetToCurrentTime()
	if success {
		lastRefreshSuccess.Set(1)
	} else {
		lastRefreshSuccess.Set(0)
	}

	if d.GetRebootRequired() {
		rebootRequired.Set(1)
	} else {
		rebootRequired.Set(0)
	}

	return success
}

// updatePatches publishes the pending patches. On failure the previous values
// are kept; updates_last_refresh_success tells they are stale.
func updatePatches(p distros.PatchLister) error {
	patches, err := p.GetPendingPatches()
	if err != nil {
		return err
	}
	for key, count := range patches {
		pendingPatches.WithLabelValues(key.Category, key.Severity).Set(float64(count))
	}
	// Series are removed one by one rather than with Reset(), so a concurrent
	// scrape never sees an empty metric.
	for key := range exportedPatches {
		if _, ok := patches[key]; !ok {
			pendingPatches.DeleteLabelValues(key.Category, key.Severity)
			delete(exportedPatches, key)
		}
	}
	for key := range patches {
		exportedPatches[key] = struct{}{}
	}
	return nil
}

func main() {
	logLevel := &slog.LevelVar{}
	logLevel.Set(slog.LevelInfo)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("Unable to load config", slog.Any("error", err))
		os.Exit(1)
	}

	var parsedLevel slog.Level
	// UnmarshalText handles string inputs like "DEBUG", "info", "Warn", "ERROR" case-insensitively
	if err := parsedLevel.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		slog.Error("Invalid log level provided, falling back to INFO", slog.String("invalid_level", cfg.LogLevel), slog.Any("error", err))
		parsedLevel = slog.LevelInfo
	}

	logLevel.Set(parsedLevel)

	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		slog.Error("Unable to marshal config", slog.Any("error", err))
	}

	slog.Debug("Application starting with config", slog.Any("config", string(cfgBytes)))

	if cfg.Version {
		fmt.Println(Version)
		os.Exit(0)
	}

	distros.UseSudo = cfg.Sudo

	distro := getDistro()
	if distro == nil {
		slog.Error("Distro not detected")
		os.Exit(1)
	}

	go func() {
		slog.Debug("Initialized go routine")

		interval := time.Duration(cfg.Interval) * time.Second
		retry := RETRY_INTERVAL
		for registered := false; ; {
			sleep := interval
			if updateMetrics(distro) {
				retry = RETRY_INTERVAL
			} else {
				// Typically the package manager being locked by another
				// process: retry sooner, backing off up to the interval.
				sleep = min(retry, interval)
				retry *= 2
			}
			if !registered {
				// Only now, so that a scrape happening during the first (and
				// possibly slow) query does not see zeroes.
				prometheus.MustRegister(securityUpdates, totalUpdates, rebootRequired, latestCacheUpdate,
					pendingPatches, lastRefresh, lastRefreshSuccess)
				registered = true
			}
			slog.Debug("Sleeping", slog.Duration("duration", sleep))
			time.Sleep(sleep)
		}
	}()

	http.Handle(cfg.TelemetryPath, promhttp.Handler())
	addr := cfg.ListenAddress
	if addr == "" {
		addr = fmt.Sprintf(":%d", cfg.Port)
	}
	slog.Info("Starting HTTP server", slog.String("addr", addr), slog.Int("interval", cfg.Interval))

	// exporter-toolkit adds optional TLS and basic authentication, see
	// https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md
	systemdSocket := false
	flags := &web.FlagConfig{
		WebListenAddresses: &[]string{addr},
		WebSystemdSocket:   &systemdSocket,
		WebConfigFile:      &cfg.WebConfigFile,
	}
	if err := web.ListenAndServe(&http.Server{}, flags, logger); err != nil {
		slog.Error("HTTP server collapsed", slog.Any("error", err))
		os.Exit(1)
	}

	slog.Info("HTTP server stopped gracefully")
}

func loadConfig() (*Config, error) {
	pflag.IntP("port", "p", DEFAULT_PORT, "HTTP port")
	pflag.String("listen-address", "", "Address to listen on, as host:port (overrides --port)")
	pflag.String("telemetry-path", "/metrics", "Path under which to expose metrics")
	pflag.String("web-config-file", "", "Path to an exporter-toolkit web configuration file enabling TLS or authentication")
	pflag.IntP("interval", "i", DEFAULT_INTERVAL, "Metrics refresh interval (seconds)")
	pflag.Bool("sudo", false, "Run apt-get and zypper through 'sudo -n' (for an unprivileged exporter)")
	pflag.BoolP("version", "v", false, "Print version")
	pflag.StringP("log-level", "l", "info", "Log verbosity level (debug, info, warn, error)")
	pflag.StringP("config", "c", "", "Config file")

	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		return nil, fmt.Errorf("unable to bind flags: %w", err)
	}

	// Environment variable matching setup
	viper.SetEnvPrefix("UPDATES_EXPORTER")
	viper.AutomaticEnv()
	// Critical: Converts dashes in flags ("log-level") to underscores for ENVs ("LOG_LEVEL")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	configFlag := viper.GetString("config")

	if configFlag != "" {
		slog.Info("Loading configuration from explicit config flag", slog.String("path", configFlag))
		viper.SetConfigFile(configFlag)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".") // Look for the file in the current working directory
		viper.AddConfigPath("/etc/updates_exporter/")
	}

	if err := viper.ReadInConfig(); err != nil {
		// It is acceptable if the config file is missing; we fall back to flags/defaults.
		// But if it exists and has a syntax error, we want to fail fast.
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			return nil, fmt.Errorf("error reading config file: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unable to decode into struct: %w", err)
	}

	return &cfg, nil
}
