# node-process-agent

A single Go binary combining node_exporter, process-exporter, and Victoria Metrics
remote_write — replacing three separate processes with one.

## Motivation

Running `prometheus-node-exporter`, `prometheus-process-exporter`, and `vmagent`
separately costs roughly 15MB of Go runtime overhead per process. On a handful of
servers this is negligible. At dozens of constrained machines it adds up.

Beyond RAM, the old architecture has an unnecessary HTTP round-trip: vmagent scrapes
the two exporters over localhost HTTP, then forwards to Victoria Metrics. A combined
binary collects metrics in-process and writes directly, eliminating the scrape loop
entirely.

## Architecture

```
/proc + /sys
     |
     v
NodeCollector.Collect()          (node_exporter collectors, in-process)
ProcessCollector.Collect()       (process-exporter collectors, in-process)
     |
     v
prometheus.Registry.Gather()
     |
     v
encode (proto3 + snappy) --> POST --> Victoria Metrics
```

No local HTTP server. No scrape loop.

On send failure the agent retries with exponential backoff (starting at 1s, capping
at 5 minutes). New metric batches queue behind the failing one, preserving ordering.
When the queue fills, the oldest batch is dropped to make room for the current
reading.

The queue is in-memory only. If the agent process exits while batches are queued
(restart, crash, reboot), those batches are lost. For node and process metrics at
15-second intervals this is an acceptable trade-off — the next collection resumes
immediately on startup. A persistent WAL is not implemented.

## Building

Requires Go 1.25 or later.

```sh
git clone https://github.com/jvasile/node-process-agent
cd node-process-agent
./dosh build
```

The `dosh` script is the project's build tool. Available commands:

| Command | Description |
|---|---|
| `./dosh build` | Build the binary in the current directory |
| `./dosh install` | Build and install to `/usr/local/bin` |
| `./dosh setup` | Clone upstream repos into `upstream/` for reference |
| `./dosh update` | Fetch upstream repos and report any new commits |

## Configuration

All configuration is via command-line flags. When running under systemd, flags are
typically passed from an environment file (see Deployment below).

| Flag | Default | Description |
|---|---|---|
| `-victoria-metrics-url` | `http://localhost:8428/api/v1/write` | Victoria Metrics remote_write endpoint |
| `-username` | _(none)_ | HTTP Basic Auth username |
| `-password-file` | _(none)_ | Path to file containing HTTP Basic Auth password |
| `-hostname` | system hostname | Value of the `instance` label stamped on every metric |
| `-interval` | `15s` | How often to collect and push metrics |
| `-queue-size` | `100` | Max unsent batches buffered during an outage |
| `-process-config` | _(none)_ | Path to process-exporter YAML config (omit to skip process metrics) |
| `-smart-interval` | `5m` | How often to re-run smartctl (set to `0` to disable SMART collection) |

### SMART metrics

SMART data is collected by running `smartctl` (from the `smartmontools` package)
inside the agent every `-smart-interval` (default 5 minutes). Results are cached
between gather cycles so the 15-second collection interval does not cause smartctl
to run on every push.

Metrics emitted (all with a `disk` label, e.g. `disk="sda"`):

| Metric | Type | Description |
|---|---|---|
| `smartmon_health_passed` | gauge | 1 = SMART health passed, 0 = failed |
| `smartmon_temperature_celsius` | gauge | Drive temperature |
| `smartmon_power_on_hours_total` | counter | Lifetime power-on hours |
| `smartmon_reallocated_sectors_total` | counter | Reallocated sector count (ATA attr 5) |
| `smartmon_pending_sectors` | gauge | Pending sector count (ATA attr 197) |
| `smartmon_uncorrectable_errors_total` | counter | Uncorrectable errors (ATA attr 198) |

`smartctl` must be installed and the agent must be able to open the raw device
files. The systemd service achieves this by adding the `disk` supplementary group.
Set `-smart-interval=0` to disable SMART collection entirely.

### Process config

The process config uses the same YAML format as
[process-exporter](https://github.com/ncabatoff/process-exporter#configuration).
Each entry names a group and describes how to match processes into it.

```yaml
# /etc/node-process-agent/processes.yml
process_names:
  - name: "{{.Comm}}"
    cmdline:
      - '.+'
```

That example groups every process by its command name. See the process-exporter
documentation for the full matcher syntax including `exe`, `comm`, and `cmdline`
with named capture groups.

## Deployment

### 1. Create a dedicated user

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin node-process-agent
```

### 2. Install the binary

```sh
go build -o /usr/local/bin/node-process-agent .
chmod 755 /usr/local/bin/node-process-agent
```

### 3. Create the config directory

```sh
mkdir -p /etc/node-process-agent
chmod 755 /etc/node-process-agent
```

### 4. Write the environment file

```sh
cat > /etc/node-process-agent/agent.env <<'EOF'
VM_URL=https://vm.example.com/api/v1/write
VM_USERNAME=myuser
VM_PASSWORD_FILE=/etc/node-process-agent/password
VM_HOSTNAME=web-01.example.com
VM_INTERVAL=15s
VM_QUEUE_SIZE=100
EOF

chmod 644 /etc/node-process-agent/agent.env
```

### 5. Write the password file

```sh
echo -n 'secretpassword' > /etc/node-process-agent/password
chmod 640 /etc/node-process-agent/password
chown root:node-process-agent /etc/node-process-agent/password
```

### 6. Write a process config (optional)

```sh
cat > /etc/node-process-agent/processes.yml <<'EOF'
process_names:
  - name: "{{.Comm}}"
    cmdline:
      - '.+'
EOF
chmod 640 /etc/node-process-agent/processes.yml
chown root:node-process-agent /etc/node-process-agent/processes.yml
```

### 7. Install and start the service

```sh
cp node-process-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now node-process-agent
systemctl status node-process-agent
```

### Checking logs

```sh
journalctl -u node-process-agent -f
```

## Upstream dependency tracking

Both node_exporter and process-exporter release regularly. Their source is kept in
`upstream/` as shallow clones for reference. Run `./dosh update` to fetch upstream
tags and compare them against the versions pinned in `go.mod`:

```sh
./dosh update
```

If a newer version is available it prints the `go get` command to update it. After
updating, test and commit the new `go.mod` and `go.sum`.
