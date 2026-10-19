---
title: "System requirements & sizing"
nav_order: 2.05
parent: Getting started
permalink: /getting-started/requirements/
---
# System requirements & sizing

What a host needs to run a lever instance, how to size the jail for the agents you run, and a
profile for small machines. The times and sizes below are typical values, not guarantees: they
depend on the disk, the network and the agent image. `lever doctor`'s *last bring-up* row shows the
total and the slowest steps of your own last `apply` or `up`, and `LEVER_TRACE=1` times every
command.

## Host requirements

| | Minimum | Recommended |
|---|---|---|
| CPU | 2 cores | 4 cores or more |
| RAM | 8 GB (jail VM 3-4 GiB) | 16 GB (jail VM 6-8 GiB, for 3 or more workers at once) |
| Free disk | 30 GB | 50 GB or more |
| Disk type | SSD. A hard disk works, but the first image load takes many minutes | NVMe SSD |
| Network | Needed for the first boot (see [what the first boot downloads](#what-the-first-boot-downloads)) and for the agents' model calls | |

The host keeps the jail VM's memory for as long as the VM runs, so leave the host itself 2-4 GB
beside it. Building scion from source takes about 4 CPU-minutes, so 2-3 minutes on 2 cores the
first time and after every scion pin change. A release build of lever downloads its release's
prebuilt scion for the pin the examples use instead
([prebuilt scion](/reference/config/#prebuilt-scion)). For another pin on a 2-core host, give lever
a prebuilt scion binary (`scion.binary`).

## Where the memory goes

All agents run in one VM (Lima) or machine (OrbStack). Its memory holds:

| Part | Memory |
|---|---|
| The Ubuntu guest itself | 0.2-0.3 GB (less with `guest_profile: small`) |
| The scion hub and runtime broker | 0.1-0.25 GB, plus its database in the page cache |
| Each agent (manager or worker): Claude Code and lever's sidecars | 0.3-0.6 GB idle; 1 GB or more in a long session |
| Each agent's work (builds, test suites, language servers) | 0.2-0.5 GB, often more |

A starting point for the jail VM's `memory`:

| Agents running at once | VM memory |
|---|---|
| Manager + 1 worker | 3 GiB at the least, 4 GiB to be comfortable |
| Manager + 2 workers | 4 GiB with `guest_profile: small` and per-agent limits, else 5 GiB |
| Each further worker | + 1 GiB, more for heavy workloads |

`memory` below 2 GiB is refused. CPU matters less than memory: agents mostly wait on the model.
Their bursts are builds and tests; scion limits each container to 2 CPUs unless you set
`resources.cpu`.

## Disk

- **The agent image** is the largest item: about 5 GB for an image built on scion's Claude harness
  image. It is stored three times: in the host's Docker store, in the jail's podman store, and once
  more when podman first prepares it for the rootless user. Budget about 15 GB for it, and more
  while an old and a new image coexist after a rebuild. The opt-in slim image
  (`make lever-image-slim`, see [building the image](/getting-started/install/)) should come to about
  1 GB, about 3 GB for its three copies, and its load and preparation shrink with it.
- **The Lima VM disk** is sparse and grows up to `disk` (default `24GiB`). The Ubuntu cloud image
  Lima caches on the host is about 0.9 GB.
- **On the host:** the Go module cache for scion (0.5-1 GB) unless you use `scion.binary`, and the
  instance's `.lever-state` (logs and ledgers, small).

On a slow disk the image's first load and preparation take minutes, and the hub's database slows
its start as it grows. An SSD is the single biggest help on an older machine.

## What the first boot downloads

- **Lima:** the Ubuntu 26.04 LTS cloud image (about 0.9 GB), once per host. **OrbStack:**
  OrbStack's Ubuntu 26.04 (`ubuntu:resolute`) machine image.
- **In the guest, with apt:** `podman`, `uidmap`, `passt`, `fuse-overlayfs`, `slirp4netns`,
  `dbus-user-session`, `iptables`, `curl` and `netcat-openbsd` when any is missing, and the
  guest release's HWE kernel (`linux-generic-hwe-26.04`) with `guest_kernel: hwe`.
- **On the host:** the Go modules of the pinned scion (skipped with `scion.binary`), or, for the
  scion pin of a lever release, that release's prebuilt scion (about 40 MB) and, with
  `remote.enabled`, its prebuilt web UI (a few MB); with `remote.enabled` and any other
  `version:` or a `source:` scion, the npm packages of scion's web UI.
- **The agent image** is never pulled from inside the jail: `make lever-image` builds it on the host
  (that build downloads its base images and Claude Code), and `lever apply` copies it in. A host
  that only runs an instance can take it as a `docker save` archive (`manager.image_tar`).
- Optional tools download their own files, e.g. the whisper models for dictation
  (`lever voice fetch`).

Under `egress: closed` the guest cannot reach the Ubuntu mirrors, so do the first apply with the
open posture or with an image and packages already in place.

## Typical bring-up times

| | Lima | OrbStack |
|---|---|---|
| First boot (new VM, packages, scion, image load) | 10-15 minutes | somewhat less: no VM image to fetch |
| `lever up` after `lever stop` (VM boot, hub start, manager resume) | 1.5-2 minutes | about 1 minute |
| `lever up` with the manager running (check and attach) | 5-15 seconds | a few seconds |
| A worker start | 10-30 seconds | 10-30 seconds |

The VM boot is most of the difference: 20-40 seconds for Lima, a few seconds for OrbStack. A
changed agent image adds its load and preparation time to the next apply, minutes on a slow disk.

## The `examples/lever-dev` sizing is not a guide

`examples/lever-dev` sets `cpus: 8`, `memory: 24GiB` and `disk: 120GiB` because its manager runs
`nested_virt` and starts whole Lima VMs inside the jail to test lever itself. An ordinary instance
needs a fraction of that; start from the tables above.

## Cap a manager that runs heavy work

Unset, `manager.resources` leaves the manager container without a memory limit. When the manager
itself runs nested VMs (`nested_virt`) or large builds and test suites, a runaway job can then use
up the whole guest: the guest's kernel kills whatever it picks (the hub, a worker, podman), or the
guest stalls or panics under memory pressure. Set
[`manager.resources`](/reference/config/#manager) so that the out-of-memory kill stays inside the
manager's container and the guest, its hub and the workers keep running:

```yaml
cpus: 8
memory: 24GiB
nested_virt: true
manager:
  resources: { memory: 18GiB, cpu: "6" }
```

For a guest sized like `examples/lever-dev` (24 GiB and 8 CPUs, a manager that starts Lima VMs
and compiles large Go packages; the example itself sets no limit), 18 GiB and 6 CPUs leave about
6 GiB and 2 CPUs for the guest, the hub and a worker. Size the limit as the guest's `memory` minus the guest, the hub and the workers
you run at once (the table [above](#where-the-memory-goes)); a nested VM's memory counts against
the manager's limit, since its QEMU runs inside the manager's container. Limit the jobs too where
you can (`GOMEMLIMIT`, a build's parallelism), so that one build does not take the manager with
it.

The limits are set when the manager is created: a change reaches it at the next
`lever up --fresh --keep-conversation` (it backs the conversation up and resumes the new manager
on it).

A guest panic that reads "Host injected async #PF in kernel mode", or guest tasks stuck in
`kvm_async_pf_task_wait_schedule`, has another cause: the host swapped the VM's memory out, and no
limit inside the guest prevents that. Since 0.50.0 lever addresses it on its own: it starts the VM
outside host swap on a Linux host and boots x86_64 guests with `no-kvmapf` (see [keeping the VM
out of host swap](/operations/#keeping-the-vm-out-of-host-swap)). From 0.51.0 one `lever stop`,
then `lever up`, applies `no-kvmapf` to a guest that does not have it yet: that bring-up reboots
the guest once. Keep `manager.resources` set anyway on a nested-virt dev instance: it is what keeps
a guest out-of-memory kill inside the manager's container.

## A small-machine profile

For a 2-core host with 8 GB of RAM and an SSD, running a manager and one or two workers (Lima):

```yaml
cpus: 2
memory: 4GiB
guest_profile: small        # trim the guest's services, add zram swap
scion:
  binary: /path/to/scion    # a prebuilt linux scion for the guest's arch: no 4 CPU-minute build
manager:
  image: lever.local/lever-claude:arm64
  resources: { memory: 1536MiB, cpu: "1.5" }
workers:
  - name: worker
    dir: workers/worker
    resources: { memory: 1GiB, cpu: "1" }
broker:
  max_running_workers: 1    # 2 if your workers are light
```

What each part does:

- **`guest_profile: small`** disables the guest services a jail does not use (snapd, unattended
  upgrades and the apt timers, multipathd, ModemManager), which saves memory and keeps apt from
  competing with a bring-up for CPU and the dpkg lock. It adds zram swap, half of the VM's memory
  compressed in RAM, so a short memory spike swaps instead of the kernel killing an agent. The
  guest then no longer installs security updates by itself: update it now and then
  (`limactl shell lever-<name> sudo apt-get update` and `... apt-get upgrade`) or recreate the
  VM. Switching back to `standard` restores what lever disabled. See the
  [`guest_profile` row](/reference/config/).
- **`resources`** caps each agent container (podman `--memory` and `--cpus`). A worker that hits
  its memory limit is killed alone and does not take the manager with it. With zram swap a
  container may also use up to its limit again in swap first (scion sets no separate swap
  limit). The limits are set when
  an agent is created: a change reaches the manager at its next
  `lever up --fresh --keep-conversation` (it backs the conversation up and resumes the new manager
  on it) and a worker at its next fresh start.
- **`broker.max_running_workers`** makes the broker refuse a worker start, resume, recycle or wake
  past the cap with a message the manager can act on (suspend or stop a worker first), instead of letting
  the VM run out of memory.
- **`scion.binary`** skips building scion on the host.
- **The slim image** (`image: lever.local/lever-claude-slim:arm64`, built with
  `make lever-image-slim`) should save about 12 GB of disk across its three copies, and minutes of
  image load on a slow disk. It carries no browser, cloud CLIs or language toolchains; see
  [the slim image](/getting-started/install/).

The guest budget for this profile is about 2-2.5 GB: the guest and hub about 0.5 GB, the manager
and one worker 0.4-0.5 GB each, their work and the page cache the rest. That leaves room in 4 GiB
for a second light worker or a memory spike.

`lever apply` and `lever up` warn when an existing VM was created with other `cpus` or `memory`:
those apply only when Lima creates the VM. To recreate it and keep the manager's conversation:
`lever conversation backup`, `lever destroy`, `lever up`, then `lever conversation restore`. `guest_profile` applies to an existing VM at the next `apply` or
`up`.

On OrbStack, the VM is OrbStack's own: set its memory and CPU limits in OrbStack's settings.
`resources` and `max_running_workers` work there too; `cpus`, `memory` and `guest_profile` are
Lima only.
