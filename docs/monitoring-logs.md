# AIStore Observability: Logs

AIStore (AIS) provides comprehensive logging that captures system operations, performance metrics, and error conditions. 

> **Scope.** How to configure, collect, and read AIS logs.

AIS logs are the cluster's ground truth: every proxy or target writes a chronological stream of events, warnings, and periodic performance snapshots. Well‑rotated logs let operators:

* Reconstruct incidents (root‑cause analysis)
* Correlate client symptoms with internal state changes
* Spot long‑running jobs without polling the control plane

## Table of Contents
- [Configuring Logging](#configuring-logging)
- [Severity & Verbosity](#severity--verbosity)
- [Log Format and Structure](#log-format-and-structure)
- [Log File Layout & Rotation](#log-file-layout--rotation)
- [Accessing Logs](#accessing-logs)
- [Common Log Patterns](#common-log-patterns)
- [Key Performance Metrics](#key-performance-metrics)
- [Troubleshooting Checklist](#troubleshooting-checklist)
- [Operational Tips](#operational-tips)
- [Related Documentation](#related-documentation)

## Configuring Logging

```bash
# Show the cluster‑wide logging section (current values)
ais config cluster log
```

| Key          | Purpose                                                                                                                            | Typical prod value |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------------- | ------------------ |
| `level`      | Info verbosity **0-5** (`3` = normal, `4`/`5` = chatty, `<3` disables *info*). `W` and `E` are **always** logged.                  | `3`                |
| `modules`    | Space‑separated list of modules whose *info* lines are forced to **level 5** (e.g., `ec space`). Use `none` to clear the override. | `none`             |
| `max_size`   | Rotate when a single file exceeds this size                                                                                        | `32MiB`            |
| `max_total`  | Upper bound for the entire directory (oldest files deleted first)                                                                  | `1GiB`             |
| `flush_time` | How often each daemon flushes its in‑memory buffer                                                                                 | `10s`              |
| `stats_time` | Interval for automatic performance snapshots                                                                                       | `60s`              |
| `to_stderr`  | Duplicate log lines to stderr (handy for systemd / kubectl logs)                                                                   | `false`            |

Show current values:

```bash
ais config cluster log               # cluster‑wide
ais config node   NODE_ID log        # single node (effective)
```

The new value propagates to every node within a second.

### Example (Development Defaults)

```json
"log": {
    "level": "3",
    "modules": "none",
    "max_size": "4MiB",
    "max_total": "128MiB",
    "flush_time": "1m",
    "stats_time": "1m",
    "to_stderr": false
}
```

### Example (Production Configuration)

In production environments, settings are typically adjusted for higher retention and less frequent statistics collection:

```
$ ais config cluster log
PROPERTY         VALUE
log.level        3
log.max_size     4MiB
log.max_total    512MiB
log.flush_time   1m
log.stats_time   3m
log.to_stderr    false
```

At startup, AIS logs some of these settings:

```
I 19:28:24.774518 config:2143 log.dir: "/var/log/ais"; l4.proto: tcp; pub port: 51080; verbosity: 3
I 19:28:24.774523 config:2145 config: "/etc/ais/.ais.conf"; stats_time: 10s; authentication: false; backends: [aws]
```

## Severity & Verbosity

AIS prepends every line with a **severity prefix** and—in the case of informational messages—an **internal numeric level**.

### Severity Prefixes

| Prefix | Meaning                              | Printed when             |
| ------ | ------------------------------------ | ------------------------ |
| `E`    | Error – unrecoverable / user‑visible | Always                   |
| `W`    | Warning – succeeded but suspicious   | Always                   |
| `I`    | Informational                        | Only if allowed by level |

### Numeric Levels for *I* Lines

| Level   | Typical use (examples)                             |
| ------- | -------------------------------------------------- |
| **5**   | Hot‑path trace, request headers, per‑part          |
| **4**   | Verbose progress, retries, caching stats           |
| **3**   | Startup, shutdown, xaction summaries **(default)** |
| **2‑0** | Progressively quieter; at `≤2` almost silent       |

> **Tip.** Temporarily crank a node:
>
> ```bash
> ais config node set TARGET log.level 4
> ais config cluster log.modules ec xs   # focus on EC & batch jobs (xactions)
> ```

### Per‑module Overrides (`log.modules`)

`log.modules` lets you boost just a subset of subsystems to level 5 without flooding the whole cluster.

```bash
# Elevate erasure‑coding (ec) and xaction scheduler (xs):
ais config cluster log.modules ec xs

# Revert to normal
ais config cluster log.modules none
```

## Log Format and Structure

AIS logs follow a consistent format:

```
I 2025‑05‑19 13:42:17.791884 cpu:60 Reducing GOMAXPROCS (prev=256) to 32
│ │            │      │              └─ message
│ │            │      └─ Go file:line inside AIS source
│ │            └─ timestamp (µs precision)
│ └─ severity prefix
└─ 'I' for INFO
```

Common prefixes:

* `config:`   – effective runtime configuration
* `x-<n>:` – extended (batch) action lifecycle
* `nvmeXnY:` – per‑disk I/O snapshot
* `kvstats:` – cluster‑wide key‑value metrics (see below)

## Log File Layout & Rotation

| Environment | Where logs appear                   | Notes                                |
| ----------- | ----------------------------------- | ------------------------------------ |
| Bare‑metal  | `/var/log/ais/<node>.log`           | One file per daemon (proxy / target) |
| Kubernetes  | container **stdout** (kubectl logs) | Collected by CRI‑O / containerd      |

File names include the node ID plus a sequence number (`target‑A43c.log.3`). Rotation is triggered by `max_size`; retention is enforced by `max_total`.

AIS implements automatic log rotation as indicated by the header:

```
Rotated at 2025/05/14 21:00:38, host ais-target-13, go1.24.3 for linux/amd64
```

When logs are rotated, new log files are created and old ones are typically compressed or archived according to the retention policy.

## Accessing Logs

### Via CLI

The AIS CLI provides commands to view and collect logs:

```bash
# View logs from a specific node
ais log show [NODE_ID]

# Filter logs by severity
ais log show [NODE_ID] --severity error

# Collect logs from all nodes
ais log get --help
```

### Directly in Kubernetes

In Kubernetes deployments, access logs using kubectl:

```bash
kubectl logs -n ais ais-proxy-15
kubectl logs -n ais ais-target-13
```

## Common Log Patterns

### Startup Sequence

The startup sequence provides important information about the AIS node configuration:

```
Started up at 2025/05/13 19:28:24, host ais-proxy-15, go1.24.3 for linux/amd64
W 19:28:24.774364 config:1506 control and data share the same intra-cluster network: ais-proxy-15.ais-proxy.ais.svc.cluster.local
I 19:28:24.774518 config:2143 log.dir: "/var/log/ais"; l4.proto: tcp; pub port: 51080; verbosity: 3
I 19:28:24.774523 config:2145 config: "/etc/ais/.ais.conf"; stats_time: 10s; authentication: false; backends: [aws]
I 19:28:24.774540 daemon:311 Version 3.28.2ec8b22, build 2025-05-13T19:20:12+0000, CPUs(32, runtime=256), containerized
```

### Operation Logs

AIS logs details about operations such as list, put, get:

```
I 21:00:43.063816 base:211 x-list[ApJcaebM5]-ais://yodas-21:00:03.062994-00:00:00.000000 finished
I 21:00:44.482430 base:211 x-list[J1qpgaWbxG]-ais://yodas-21:00:04.481894-00:00:00.000000 finished
```

### Performance Metrics

AIS regularly logs performance metrics in two formats:

1. Disk-specific performance:

```
I 21:00:48.784074 nvme3n1: 54MiB/s, 119KiB, 0B/s, 0B, 26%
I 21:00:48.784078 nvme9n1: 41MiB/s, 119KiB, 0B/s, 0B, 20%
I 21:00:48.784080 nvme10n1: 38MiB/s, 112KiB, 0B/s, 0B, 18%
```

2. Comprehensive key-value statistics (at regular intervals defined by `stats_time`):

```
I 18:06:18.785011 {aws.head.n:114227,aws.head.ns.total:16799090100532,del.n:109,err.get.n:296108,err.ren.n:1,etl.offline.n:1136785,etl.offline.ns.total:73240613148414,get.bps:219094016,get.n:1006219,get.ns:425545477639,get.ns.total:3041645043551487925,get.redir.ns:3262104,get.size:126049578974910,lcache.evicted.n:1211669,lcache.flush.cold.n:491970,lst.n:8824,put.n:286,put.ns.total:1706491834824,put.size:101717912588,ren.n:103,state.flags:32774,stream.in.n:441,stream.in.size:96717189120,stream.out.n:446,stream.out.size:84119388160,disk.nvme7n1.read.bps:35240346...}
```

### Kubernetes-specific Information

In Kubernetes deployments, AIS logs include pod and cluster-specific details:

```
I 19:28:24.786264 k8s:93 Pod info: name ais-proxy-15 ,namespace ais ,node 10.49.41.55 ,hostname ais-proxy-15 ,host_network false
I 19:28:24.786281 k8s:101   ais-ais-state (&PersistentVolumeClaimVolumeSource{ClaimName:ais-ais-state-ais-proxy-15,ReadOnly:false,})
I 19:28:24.786304 k8s:103   config-template
I 19:28:24.786306 k8s:103   config-mount
```

## Key Performance Metrics

The key-value statistics contain valuable operational metrics:

| Key / pattern | Description |
|--------|-------------|
| `get.n` | Number of GET operations |
| `put.n` | Number of PUT operations |
| `get.size`, `put.size` | Cumulative bytes, GET and PUT respectively |
| `get.bps` | Bytes per second for GET operations |
| `aws.<name>.ns.total` | Cumulative latency against a cloud backend (AWS S3, in this example) |
| `aws.head.n` | Number of HEAD requests to AWS S3 |
| `err.get.n` | Number of GET errors |
| `disk.<device>.read.bps` | Read throughput for specific disk |
| `disk.<device>.util` | Device utilization percentage |

## Troubleshooting Checklist

1. Scan for **E** & **W** lines around the timeframe.
2. Look for spikes in `err.<n>.n` counters.
3. Watch disk `util` > 80% or sustained `read.bps` plateaus.
4. Temporarily raise `log.level` or `log.modules` on a single node to capture more detail.

For advanced log analysis, consider forwarding logs to external systems for aggregation and visualization.

## Operational Tips

* Keep `log.level=3` in production; raise to `4` or `5` only while debugging. Lower to `2` or below if you truly need silence.
* Raise `stats_time` (≥ 60s) if logs get noisy on busy systems.
* Ship rotated logs off‑host weekly.
* Always attach `ais cluster download-logs` tarball to GitHub issues.

## Related Documentation

| Document | Description |
|----------|-------------|
| [Overview](/docs/monitoring-overview.md) | Introduction to AIS observability |
| [CLI](/docs/monitoring-cli.md) | Command-line monitoring tools |
| [Prometheus](/docs/monitoring-prometheus.md) | Configuring Prometheus with AIS |
| [Metrics Reference](/docs/monitoring-metrics.md) | Complete metrics catalog |
| [Grafana](/docs/monitoring-grafana.md) | Visualizing AIS metrics with Grafana |
| [Kubernetes](/docs/monitoring-kubernetes.md) | Working with Kubernetes monitoring stacks |
