# Prometheus Updates Exporter

This is a Prometheus exporter that exposes system package update statistics:

| Metric | Description |
| --- | --- |
| `updates_pending` | Total number of pending package updates (`-1` if the package manager could not be queried) |
| `updates_pending_security` | Number of pending security updates (`-1` on failure). Packages on apt and dnf, patches on zypper |
| `updates_pending_patches{category,severity}` | Number of pending patches by category (`security`, `recommended`, `optional`, `feature`, ...) and severity (`critical`, `important`, `moderate`, `low`, `unspecified`). zypper only |
| `updates_reboot_required` | 1 if a reboot is required (due to package updates), 0 otherwise |
| `updates_latest_cache_change` | Unix timestamp of the last refresh of the package manager's metadata |
| `updates_last_refresh_timestamp_seconds` | Unix timestamp of the last time the exporter queried the package manager |
| `updates_last_refresh_success` | 1 if that query succeeded, 0 otherwise |

The package manager is queried at startup and then every `--interval` seconds
(one hour by default), not on every scrape. A failed query (e.g. zypper being
locked by another process) is retried after one minute, then two, four...

RHEL derivatives, Ubuntu, Debian, openSUSE and SLES are supported. Other dnf,
apt and zypper-based distros probably work fine.

| Distro family | Source |
| --- | --- |
| Ubuntu, Debian | `apt-get -s dist-upgrade`; an update is a security update when its candidate comes from a `*-security` archive or from `Debian-Security` |
| RHEL | `dnf updateinfo list` |
| openSUSE, SLES | `zypper --xmlout list-patches` and `zypper --xmlout list-updates --type package` |

The exporter never runs `apt-get update` or `dnf makecache`: something else
(e.g. `apt-daily.timer`, `dnf-makecache.timer`) has to keep the metadata fresh,
and `updates_latest_cache_change` tells whether it does. zypper refreshes the
repositories having `autorefresh` enabled by itself when the exporter runs as
root.

On zypper, patches blocked by a pending update of the package manager itself
are counted as well.

# Installation

The easiest way is to install a prebuilt RPM package:

```
$ wget https://puppeteers-public.hel1.your-objectstorage.com/updates_exporter-0.0.7-1.x86_64.rpm
$ rpm -i updates_exporter-0.0.7-1.x86_64.rpm
$ systemctl enable updates-exporter
$ systemctl start updates-exporter
```

Alternatively build yourself or get a precompiled executable from the GitHub
Releases page. You can use
[podman-builder](https://github.com/Puppet-Finland/podman-builder) to build
your own package.

# Usage

Updates Exporter listens on port 9101. Just scrape /metrics and you're good to go.

| Flag | Default | Description |
| --- | --- | --- |
| `--port`, `-p` | `9101` | HTTP port |
| `--listen-address` | | Address to listen on, as `host:port` (overrides `--port`) |
| `--telemetry-path` | `/metrics` | Path under which to expose metrics |
| `--web-config-file` | | [exporter-toolkit web configuration file](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md) enabling TLS or authentication |
| `--interval`, `-i` | `3600` | Metrics refresh interval (seconds) |
| `--sudo` | `false` | Run `apt-get` and `zypper` through `sudo -n`, see below |
| `--log-level`, `-l` | `info` | Log verbosity level (debug, info, warn, error) |
| `--config`, `-c` | | Config file; defaults to `config.yaml` in the working directory or in `/etc/updates_exporter/` |

Every flag can also be set in the config file, or through an environment
variable (`UPDATES_EXPORTER_LISTEN_ADDRESS`, `UPDATES_EXPORTER_LOG_LEVEL`, ...).

# Running unprivileged

`apt-get -s` and `zypper list-*` work as an unprivileged user, but zypper then
cannot refresh the repositories. With `--sudo` the exporter runs the package
manager through `sudo -n`, which needs sudoers rules for the exact commands:

```
# /etc/sudoers.d/updates_exporter
updates-exp ALL = (root) NOPASSWD: /usr/bin/zypper --quiet --non-interactive --xmlout list-patches
updates-exp ALL = (root) NOPASSWD: /usr/bin/zypper --quiet --non-interactive --xmlout list-updates --type package
# apt-based distros (optional, apt-get -s does not need root):
updates-exp ALL = (root) NOPASSWD: /usr/bin/apt-get -s -o Debug\:\:NoLocking\=true dist-upgrade
```

Unprivileged, apt only warns when it cannot read a file of `/etc/apt`
(e.g. a `0600` file in `preferences.d` or `apt.conf.d`) and simulates the
upgrade without it, which can report every package of a pinned-down
repository. The exporter then reports a failure (`-1`) and logs the files
concerned: make them world-readable or use `--sudo`.

sudo does not work in a systemd unit setting `NoNewPrivileges=yes`, or one of
the options implying it for a non-root `User=` (`ProtectKernelTunables=`,
`ProtectKernelModules=`, `RestrictSUIDSGID=`, ...).

# Example alerts

```yaml
- alert: SecurityUpdatesPending
  expr: updates_pending_security > 0
  for: 3d
- alert: CriticalPatchesPending
  expr: sum without (category) (updates_pending_patches{severity="critical"}) > 0
  for: 1d
- alert: UpdatesExporterStale
  expr: updates_last_refresh_success == 0 or time() - updates_latest_cache_change > 3 * 86400
  for: 6h
```
