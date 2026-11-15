# Reference

## Commands

| Command | Does |
| --- | --- |
| `virticle [flags]` | Runs the controller. |
| `virticle version` | Prints version, commit and build date. |
| `virticle setup [--address ADDR] [--port N] [--check] [--undo] [--no-firewall] [--yes]` | Run once on the controller's machine after installing, as root (`sudo virticle setup`). On a terminal it asks which address and port the web interface uses (defaults: all addresses, 8463). `--address` (an IP address of this machine, or `all`) and `--port` answer those questions; they only apply while no administrator exists. It checks that the port is free, asks before it opens the port in firewalld or ufw (without a terminal it opens it, unless `--no-firewall`), makes sure the service runs and prints the address to open. `--yes` asks nothing and answers yes. `--check` changes nothing; `--undo` removes the firewall rule it added. Takes `-data`. See [install.md](install.md#setup-choose-the-address-and-port-open-the-firewall). |
| `virticle node prepare\|unprepare` | Prepares a host as a node, or undoes it. See [install-node.md](install-node.md). |
| `virticle backup [-o FILE\|DIR] [--encrypt] [--passphrase-file FILE] [--keep-last\|--keep-hourly\|--keep-daily\|--keep-weekly\|--keep-monthly N]` | Writes the controller's own data (database, SSH key, certificates) to one file, while the controller runs. With `-o DIR` and `--keep-*` flags it then deletes the older backups in DIR that the [retention rules](#retention) do not keep; without them it deletes nothing. Takes `-data`. |
| `virticle restore [--key FILE] FILE` | Puts such a file back; the controller must be stopped. `--key` names the recovery key file (`virticle-backup-key.age`) for a scheduled copy encrypted to the backup key; without it the key in the data directory is used. Takes `-data` and `--passphrase-file`. See [controller-backup.md](controller-backup.md). |
| `virticle web show\|set\|reset` | Reads or changes the stored web access settings. `show` and `reset` take only `-data`. See [web-tls.md](web-tls.md). |
| `virticle user reset-2fa\|reset-password USER` | Recovery on the controller's machine: turn two-step sign-in off for a user, or set a new password for a local account. Takes `-data` and, for `reset-password`, `--password-stdin`. See [sign-in.md](sign-in.md#two-step-sign-in-authenticator-app). |

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data` | `/var/lib/virticle` if it exists and holds a database, else `data` | Directory for the database and the controller's SSH key. Also for `virticle web` and `virticle node`. |
| `-listen` | none | Address of the web UI and API. Given, it overrides what the **Web access** page stores and makes that page read-only. Leave it out to manage web access from the UI. |
| `-version` | | Print the version, commit and build date and exit (same as `virticle version`). |
| `-no-scheduler` | off | Do not run scheduled snapshots and backups from this controller. |
| `-alert-webhook` | empty | Deprecated. URL that receives a JSON POST when an alert is raised or cleared. Given, it is listed under **Settings > Alerts** as one more destination that cannot be edited. |

Without `-listen` and without stored web settings, a data directory with no
users serves HTTPS with a self-signed certificate on port 8463 of all
interfaces; one that already has users keeps plain HTTP on `127.0.0.1:8080`.
Stored settings and the `-listen` flag keep the port they name.

Deprecated, hidden from `-help`, and still working with a notice in the log:
`-tls-cert`, `-tls-key`, `-insecure-http`, `-trusted-proxy` (use the **Web
access** page or `virticle web set`), `-alert-webhook` (use **Settings > Alerts**), and `-alert-pool-warn` (85),
`-alert-pool-critical` (95), `-alert-memory` (90), `-alert-cert-days` (30),
whose defaults stay.

## Environment

| Variable | Meaning |
| --- | --- |
| `VIRTICLE_LISTEN` | Default for `-listen`. |
| `VIRTICLE_JUMP_PASSWORD` | Password for node jump hosts. |
| `VIRTICLE_UPGRADE_LISTEN` | Set by the container image to `0.0.0.0:8080`: a data volume from 0.2.0 (users, no stored web settings) keeps answering there, as before. Packages and binaries do not set it. |

The package reads these from `/etc/virticle/virticle.env`. Deprecated like
their flags: `VIRTICLE_TLS_CERT`, `VIRTICLE_TLS_KEY`, `VIRTICLE_INSECURE_HTTP`,
`VIRTICLE_TRUSTED_PROXY`, and `VIRTICLE_VIRTIO_WIN_PATTERN` (volume name
pattern that counts as the virtio-win ISO).

## Roles

People can also sign in through an identity provider or a directory, and local accounts can use two-step sign-in; see [sign-in.md](sign-in.md). `virticle user reset-2fa USER` and `virticle user reset-password USER` are the shell recovery commands.

| Role | Can |
| --- | --- |
| `viewer` | Read everything except the audit log. Cannot open a console. |
| `operator` | Viewer, plus VM, storage, network, snapshot, backup, migration and console actions. |
| `admin` | Operator, plus nodes, users, web access and the audit log. |

- A route that is not listed in `internal/api/access.go` requires `admin`.
- Failed sign-ins are rate limited per user and per address.

## API tokens

1. Create a token under **Account**. The secret is shown once.
2. Give it a role (yours or lower) and an optional expiry.
3. Send it as a bearer token:

       curl -H "Authorization: Bearer virticle_xxxxxxxx" https://vm.example.org/api/vms

       curl -X POST -H "Authorization: Bearer virticle_xxxxxxxx" -H 'Content-Type: application/json' \
            -d '{"action":"start"}' https://vm.example.org/api/nodes/1/vms/<uuid>/action

Token requests are written to the audit log. Account settings need a web
session, not a token.

## Alert webhook body

A destination of type **Webhook** receives a `POST` with `Content-Type: application/json` and
`User-Agent: virticle-alerts` for every alert that is raised (or changes severity) and every alert that clears:

```json
{"event":"raise","alert":{"id":"pool:1:data","kind":"pool-usage","severity":"critical","title":"Pool data is 97% full","message":"Storage pool data on kvm1 uses 970 GiB of 1000 GiB.","node":1,"link":"/storage","since":1700000000},"time":1700000100}
```

- `event`: `raise`, `clear`, or `test` (the **Send test message** button; its alert has `"id":"test"`).
- `alert.kind`: `node-down`, `pool-usage`, `host-memory`, `failed-job`, `vm-state`, `tls-expiry`,
  `scheduler-conflict`, `net-link`, `net-bond`, `net-drift`, `storage-unavailable`, `rolling-update`, `replica-age`, `stale-copy`. `severity` is `warning` or `critical`. `node` is
  the node id and `link` the page in the web UI (both may be missing); `since` and `time` are Unix seconds.
  A `failed-job` alert of a task that a person started (not the scheduler) carries `user` and is shown in the web UI
  only; destinations receive the failed-job alerts of scheduled runs.
- One alert per request, always; the webhook is never summarised. Any 2xx answer is success. Other answers are
  retried after 5 s, 30 s and 2 min, except 401, 403, 404, 410 and other 4xx (not 408, 429), which are not.
  On 429 the `Retry-After` header is honoured.

With a **Secret** set, the request has the header `X-Virticle-Signature: sha256=<hex>`, where `<hex>` is the
lowercase hex of HMAC-SHA256 over the exact request body with the secret as key. To verify, compute the same over
the raw bytes you received (before parsing) and compare in constant time:

```python
import hmac, hashlib
def valid(secret: bytes, body: bytes, header: str) -> bool:
    want = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, header)
```

The API for the Alerts page: `GET/POST /api/notify/destinations`, `PUT/DELETE /api/notify/destinations/{id}` and
`POST /api/notify/destinations/{id}/test` (admin). The address is accepted on create and update and never
returned: the list carries `host` and `tail` (the last four characters). `GET`/`PUT /api/alerts/settings` of
earlier versions no longer exist.

## Where state lives

- **libvirt on the nodes**: VMs, networks, pools, volumes, snapshots. Per-VM
  app data (template mark, tags, schedules, CPU model marker) sits in one
  `<metadata>` element of the domain XML, namespace
  `https://virticle.invalid/ns/1`.
- **The data directory** (`-data`):
  - `virticle.db`: users, password hashes, sessions, token hashes, node list,
    web access settings, alert destinations (addresses and secrets encrypted), task log, audit log, the state of the rolling update,
    and the work directories of replication runs and backups that are not removed yet (see "Work directories" below).
  - `virticle.db.bak-<version>-<time>`: copy made before a schema upgrade
    (newest 3 kept).
  - `metrics.db`: the performance history of VMs and nodes, up to 7 days
    (see [metrics.md](metrics.md)). It is not part of the schema-upgrade copy
    and can be deleted at any time; only the history is lost.
  - `id_ed25519`, `id_ed25519.pub`: the controller's SSH key.
  - `known_hosts`: node host keys, trusted on first use.
  - `tls-cert.pem`, `tls-key.pem`, `web-own/`, `acme/`: certificates.
  - `instance-id`: stamped into scheduled snapshots.
  - `secret.key`: the one key that encrypts every stored secret (two-step sign-in seeds, identity provider and
    directory passwords, alert webhook addresses and signing secrets), created on first use, mode 0600. Keep it with
    the database; without it those secrets must be entered again. An older `settings.key` is renamed to it.
  - `migration-ca/`: the certificate authority of encrypted migration.
  - `backups/`: the daily controller backups (when switched on); `last-backup.json`: the time of the last backup; `.virticle.lock`: held while the controller runs.
  - `virticle backup` writes everything above that cannot be rebuilt into one file; see [controller-backup.md](controller-backup.md).
- **Memory only**: the newest statistics of every node and VM (the live
  charts show the last 10 minutes of them, and `/metrics` serves the latest),
  alert state, rate-limit counters.

## New VM defaults

- **CPU**: the newest model every online node can run (`custom`, exact match),
  so the VM can live-migrate anywhere in the cluster. `host-model` and
  `host-passthrough` can be chosen instead.
- **Clock** (Linux): `rtc` catch-up, `pit` delay, `hpet` off, `kvmclock` on.
  The guest clock is synced after resume, snapshot revert and live migration
  when the guest agent runs.
- **Clock** (Windows): `localtime` (the hardware clock in local time, as Windows expects), `hypervclock` instead of
  `kvmclock`. QEMU fills that clock with the node's local time, UTC on most nodes, and what the guest writes into it
  lasts only until the VM is powered off. So after a start from Virticle (start, restore with start) the guest clock is
  synced as soon as the agent answers (up to 5 minutes), when it is more than 2 seconds off: a Windows guest in another
  time zone would otherwise run hours off until Windows corrects itself. A VM started by the node at boot (autostart) is
  not synced.
- **Serial console**: added to every new VM; the Console card on the Overview adds one to a VM that has none.
- **Firmware** (Linux): UEFI, with the UEFI variables in a qcow2 file so the VM can take internal snapshots. A node without UEFI firmware gets BIOS. Windows always gets UEFI.

## Migration network and limit

- Each node has a migration network: the address other nodes send migration data and file copies to. Default: the
  management address. `GET` and `PUT /api/nodes/{id}/migration-network` read and change it (`address`, and `limit`).
- Limit on a shared link, per node: `""` (default, 80% of the link speed when known, else none), `"none"` or
  `"custom:<Mbit/s>"`. It applies only while migration data shares the interface of the management address. The lower
  limit of the two nodes wins. `bandwidthMiB` in a migrate request replaces it.

## Schedules

- Stored in the VM's metadata and run by the controller. You enter and see times in your browser's time zone; they are stored in UTC.
- **Snapshots**: interval or daily, keep newest N. Shut-off VMs are skipped.
  Only snapshots the scheduler made are pruned.
- **Backups**: interval or daily, target node and pool, and [retention](#retention) rules.
- **Replication**: every 5 minutes up to 1 day to one standby node ([Replication](#replication)).
- A failed run of a backup or a replication is retried after 5 minutes and raises an alert.
- Run one controller per set of nodes. With several, start all but one with
  `-no-scheduler`.
- Two controllers of different versions must never manage the same nodes; see [Versions and the node command](#versions-and-the-node-command).

## Replication

How to use it: [user-guide.md](user-guide.md#replication-and-failover).

### API

| Call | Role | Does |
| --- | --- | --- |
| `GET`, `PUT`, `DELETE /api/nodes/{id}/vms/{uuid}/replication` | `GET` viewer, others operator | Read, set up or change, stop. `PUT` takes `{targetNode, everyMinutes}`; with `replaceStale: true` it replaces the old copy on the standby (the **Replicate back** button). |
| `POST .../replication/run`, `.../replication/cancel` | operator | Copy now; stop a running copy. |
| `GET`, `DELETE .../replica` | `GET` viewer, `DELETE` operator | Read or delete the replica on the standby. |
| `GET .../replica/failover-check` | viewer | The checks before an unplanned failover, run from the standby (up to about 7 seconds): `{refuse?, warning?, checked, answers?}`. Empty `checked` when the failover is not unplanned. |
| `POST .../replica/failover` | operator | `{"mode":"planned"\|"unplanned", "startAnyway"?}`. Refused with 409 when the situation changed since the page was read, when the source node answers to the standby with the VM running (`refuse`), or when the VM answers on the network and `startAnyway` is not true (`warning`). |
| `DELETE .../vms/{uuid}?replica=1` | operator | Delete the VM and its replica. |

Task kinds: `replicate`, `failover`, `replication stop`, `replica delete`. Alerts: `replica-age` (the copy is older than
twice its interval plus a minute; clears when a run succeeds) and `stale-copy` (an old copy exists). A failed run is retried
after 5 minutes, is recorded as a failed task and raises `failed-job` until a run succeeds.

### What is stored

- In the source VM's metadata, the child `replication`: standby node id and name, pool, `every-minutes`, replica directory,
  last result. The standby's domain has the child `replica`. After a failover the old VM has `stale-copy` and the new one
  `failed-over`. Older versions ignore these.
- The replica directory is a hidden work directory `.virticle-rp.XXXXXX` in a pool directory of the standby. Per disk it
  holds a base `b<G>-<disk>.qcow2` (generation G, a full copy) and one overlay `p<S>-<disk>.qcow2` per point S, each holding only
  what changed since the point before. `p<S>.json`, written last and never changed, makes point S exist. The newest 2 points
  are kept; older ones are written into the base, oldest first. A full copy is a new generation, and older generations go
  only after its first point is complete. Cutting any step short leaves a usable replica; the next run or the failover
  finishes the tidying.
- Checkpoints on the source have the description `replica:<standby node id>`. Backups count and delete only their own (at
  most 4); replication keeps exactly one. Snapshot revert, migration and disk move drop both.

### A run

1. A backup job from the checkpoint (a full one when there is none) is sent over the migration network and checked by SHA-256.
2. The overlay is linked with a header-only rebase, the manifest is written, old points are merged, and the old checkpoint is
   deleted.
3. A run is full when there is no point, the replica is damaged, the checkpoint is gone (killed QEMU, migration, revert,
   restore), or the disk layout changed.

Rules:

- One run per VM and per source node at a time. A backup and a run of one VM exclude each other.
- The scheduler loop starts due runs every 20 seconds. `-no-scheduler` runs none; `POST .../replication/run` does one.
- A shut-off VM is copied only when its disk files changed since the last point (mtime and size). If the copy can be
  incremental, the VM is started paused (the vCPUs never run) and the same job runs; QEMU is then ended with a graceful destroy
  so that it saves its bitmaps. The `replication` child carries `paused-copy="yes"` meanwhile, Start and Resume are refused, and
  a controller restarted in between ends the paused VM in its next round. Any other case converts the disks whole and starts
  nothing.
- Refused: raw disks, shareable or non-file disks, passthrough devices, a standby with another VM of that name or UUID, a
  missing network or bridge on the standby, no pool with room, and a VM with any writable disk in a directory that the standby
  mounts as the same NFS share. The replica is never put in an NFS pool.
- TPM: the state of an emulated TPM 2.0 (one small file under `/var/lib/libvirt/swtpm/<uuid>/`) is copied with every run (node
  verb `tpm-save`) and put back at failover before the disks move (`tpm-place`). Other TPMs are not copied (warning at setup); the
  VM then starts with a new, empty TPM.

### Failover and the old copy

- Failover uses the newest point whose chain reads. Its overlays are written into the base, the base is moved to the pool as
  `<vm>.qcow2` (`<vm>-<disk>.qcow2` for more disks), the UEFI variables are placed, the definition of that point is defined,
  and the VM starts. It can be repeated after any failure.
- Order is decided by a generation counter. A VM that takes over gets `failed-over gen="N+1"`, N being the generation of the
  source (the replica's `src-gen`, copied at every run from the source's own `failed-over`). The old VM is marked and powered
  off only when its own generation is lower (or it has none); equal generations do nothing. `at` (the controller's clock) is
  informational only. The decision is `decideOldCopy` (`replica_reconcile.go`), a pure function with a table test. The
  controller marks the old VM when its node is online again: at once when the node's connection comes up
  (`reconcileOnReconnect`), and in the alert loop once a minute.
- Start guard on the old copy. The `stale-copy` child holds `gen` (the failover generation the old VM had) and, when known,
  `peer-host`, `peer-port`, `peer-user` (copied from the `replication` record it replaced). `hook-qemu` asks that node
  `took-over UUID GEN` exactly as for a replicated VM; no address means no question and the VM starts.
- AppArmor (Debian, Ubuntu). libvirtd runs the hook in its own profile, which does not allow the node command. `node
  prepare` adds a block between `# BEGIN virticle start guard` and `# END virticle start guard` to
  `/etc/apparmor.d/local/usr.sbin.libvirtd` (a child profile `virticle_node`: ssh, getent, the account's `~/.ssh` files,
  TCP) and reloads the profile; `unprepare` removes it. The probe VM runs on every prepare.
- Start guard. The `replication` child also holds `peer-host`, `peer-port`, `peer-user` (the standby as the source node
  reaches it; written at every run), the `replica` child `addrs="MAC=ip,ip;..."` (the VM's IPv4 addresses at the last copy,
  from the guest agent, else DHCP leases). `node prepare` installs `/etc/libvirt/hooks/qemu.d/virticle-guard`, a
  `#!/bin/sh` file that runs `virticle-node hook-qemu ACCOUNT "$@"` and maps its exit status 13 to a failure (anything else,
  or a missing command, lets the VM start). On `prepare begin` the verb reads the domain XML from stdin; without a
  `replication` child it exits at once. Otherwise it asks the standby over the node key (`peer-recv` header
  `took-over UUID GEN`, GEN being the VM's own `failed-over` generation, 0 if none): the standby answers from its libvirt
  metadata, "took over" when its `failed-over` generation is higher (`nodeproto.TookOver`, the rule of `decideOldCopy`).
  Ask limit 5 seconds, hard limit 7. Libvirt reads the hook directory at start and on SIGHUP, so prepare reloads
  `virtqemud`/`libvirtd` (running VMs are not touched). Prepare starts a probe VM `virticle-guard-selftest` that the hook
  refuses with a line naming what it may do (`exec`, `ssh`, `key`, `net`); a hook that libvirt cannot run is removed
  again. On Rocky 10 `virtqemud_t` is a permissive SELinux domain: the
  hook runs, and denials (execute of the hook file, TCP connect to port 22) are only logged.
- Checks before an unplanned failover (`replica_checks.go`; the decision is the pure `decideFailoverChecks`): node verb
  `peer-ask HOST PORT USER vm-state UUID` (run on the standby; answers from the source node's libvirt, over the standby's
  node key, which is installed on the source when replication starts) and `vm-probe BRIDGE IP...` (an ARP request with
  `arping`, else ping and `ip neigh`; an answer counts only from the card's own MAC). The standby needs an IPv4 address on
  the bridge to ask from. Duplicate address detection is not used: in the lab a guest dropped its address after two such probes. Only cards on a bridge
  the standby has too are probed (a `type=bridge` card, or a libvirt network in `bridge` mode).
- Moving a replicated VM to its standby: the replica's domain is undefined (its directory stays), the source's `replication`
  child and its checkpoints are removed, the migration runs, then the old directory is deleted and replication to the old
  node is set up (full copy). A failed migration puts the replica domain and the setting back. Deleting a node switches off
  replication to it on the other nodes.

### Damage and leftovers

- A `repl-commit` that was killed leaves refcounts behind the data. The verb repairs with `qemu-img check -r all`, commits
  again (which rewrites every block of the overlay, so the base equals the overlay over the old base whatever the first run
  wrote) and requires a clean `qemu-img check`. Otherwise it fails with `replica-damaged`; the next run clears the replica
  directory and copies whole, and a failover refuses with the advice to delete the replica. Lab: 40 rounds of `kill -9` at
  random points, repair, commit, `qemu-img compare` against the view taken before the commit: all identical.
- A persistent `virticle-` or `backup-` bitmap with the `in-use` flag in an image that no QEMU has open (a killed QEMU) is
  removed with `qemu-img bitmap --remove` (node verb `qcow2-bitmap-rm`, which takes the image lock and so fails on a running
  VM), or with `block-dirty-bitmap-remove` for a bitmap a running QEMU calls inconsistent. This happens before a disk resize,
  and when a checkpoint could only be deleted as metadata. Their checkpoint records are deleted with them.
- Orphan replica directories that a stopped controller left are swept (node verb `work-ls`).
- Work directories of runs that were cut short are removed; see "Work directories" below.

### Work directories

What a replication run or a backup makes on a node, and when it goes:

| Directory | Node | Normally removed |
| --- | --- | --- |
| `.virticle-rp.XXXXXX`, staging of a replication run | source | at the end of the run |
| `.virticle-rp.XXXXXX`, the replica directory | standby | never by a run (it is the copy); "stop replication" removes it |
| `in<S>-...` files inside the replica directory | standby | by the next run (it tidies the replica first) |
| `.virticle-bk.XXXXXX`, staging of a backup | source | after the copy to the target, or at the end |
| `.virticle-bk.XXXXXX`, the backup being written | target | renamed to the backup when it is complete |

A run that dies with its node, or with the controller, cannot remove them. So the controller records each one in its
database when it makes it, and drops the record when the directory is gone. When a node comes online, and before a
replication run or a backup is planned on it, the recorded directories of that node are checked:

- A directory a run of this controller uses right now is never touched.
- A directory that is not recorded is never touched by this check: it may belong to another controller.
- A directory from a run of the running controller that has ended is removed at once.
- A directory recorded before the controller was restarted is removed once nothing in it has changed for 10 minutes
  (libvirt may still be writing a backup job's files there; the check is repeated by itself after that time).
- A new replica directory is recorded only until the VM's replication setting names it. A recorded one is removed only
  when no replica and no setting on any node names it (all nodes must answer) and it holds no point and no base image.
  A directory with a copy in it is never removed by this check.

Each removal is logged with the path, the size and the number of files. The older checks for directories nobody
recorded stay as they were: replica directories nothing names after 12 hours, backup directories after an hour without
a change.

## Backup format

A directory `<vm>-<UTC timestamp>.vmbackup` in a directory-backed pool:

- `manifest.json`: VM name and UUID, time, source node, disks, SHA-256 checksums (always for a backup of a running VM with qcow2 disks, since it can become part of a chain; a restore checks every file it reads).
- `domain.xml`: the VM definition.
- `<target>.qcow2`: one flattened image per writable disk. In an incremental backup it holds only the blocks that changed
  since the parent, and has no backing file.
- `nvram.fd`: for UEFI guests; the manifest records its format (`nvramFormat`: `qcow2`, or `raw` for VMs made before qcow2 stores).
- `tpm2-state.bin`: the state of an emulated TPM 2.0 (swtpm's `tpm2-00.permall` from `/var/lib/libvirt/swtpm/<uuid>/tpm2/`,
  node verb `tpm-save`), whole in every backup, incremental ones too. The manifest names it in `tpmState` with `tpmBytes` and
  `tpmSha256`. It is absent for a VM without a TPM, a TPM 1.2 or passthrough TPM, a TPM that has no state yet (the VM never
  ran), a state the node could not read, and for backups from before this field. The manifest format number did not change:
  an older Virticle ignores the field and restores such a backup with an empty TPM; it refuses to import an export that
  contains the file. A restore checks the file against `tpmSha256` on the target node, defines the VM and then places the
  state under the VM's UUID (a new one for a restore as a new VM) with `tpm-place`, the verb a failover uses; libvirt labels
  it for SELinux or AppArmor when the VM starts. The notes say "TPM state is included" or "TPM state is not included"; the task
  of a backup of a VM with a TPM whose state is not included ends with the note "TPM state not included: REASON".

A running VM is backed up with libvirt's backup API and keeps running. The
directory is renamed into place last, so a backup is complete or absent. A
backup restores as a new VM or as the original, and exports to one
`.vmexport.tar`.

**Starting a backup.** `POST /api/nodes/{id}/vms/{uuid}/backup` with `{targetNode, pool, keepLast}` answers
`{task, mode, plannedKind, plannedReason}`. `mode` is `live` or `offline`. `plannedKind` (`full` or `incremental`) is only
what the run will try, and `plannedReason` says why a full backup is planned. libvirt can still refuse an incremental
backup, and the run then makes a full one, so a script must not take `plannedKind` as the result. Read the kind of what
was made from `manifest.kind` (and `manifest.fullReason`) of the entry in `GET /api/backups?uuid=UUID`.

**Verifying a backup.** `POST /api/nodes/{id}/backups/{pool}/{name}/verify` (operator) answers `{task}`. The task, kind
`verify`, runs on the node that holds the backup and reads the backup and every backup it builds on, full one first:

- Every file of each manifest must be there: `manifest.json`, `domain.xml`, the UEFI variables, the TPM state, the disks.
- Files with a recorded size (disks, TPM state, encrypted UEFI variables) must have it; files with a recorded SHA-256
  must match it (node verb `sha256 PATH progress`; progress by bytes read). Encrypted files are checked as stored
  (`bytes` and `sha256` are those of the `.age` file), so no passphrase is needed.
- It ends ok with a note such as "9 files intact in 3 backups" (plus "N without a checksum, size checked only" for disk
  images that have none), or fails with the first missing or damaged file, or the first missing earlier backup.
- It changes nothing, can be stopped (`POST /api/tasks/{id}/cancel`), and retention keeps the chain while it reads.

**Freezing the guest.** libvirt's backup-begin has no flag to quiesce the guest (the value 2 that earlier builds passed is
rejected by libvirt 11.3 and means "preserve the domain if the guest shuts down" in 11.10). For a running VM Virticle freezes
the guest's file systems with libvirt's `DomainFsfreeze` (the guest agent's `guest-fsfreeze-freeze`) immediately before
`DomainBackupBegin` and thaws them (`DomainFsthaw`, up to 4 tries) when it returns, whether it worked or not. An agent that does
not answer `guest-info` within 5 seconds is not asked to freeze. The backup waits up to 2 minutes for the freeze; after that
it is made without ("not frozen: the guest did not freeze within 2 minutes"). libvirt cannot cancel a freeze and runs one
agent command per guest at a time, so the freeze goes on, and when it takes effect later Virticle thaws the guest at once
(logged). A Windows guest freezes through VSS: the agent prepares the snapshot and then waits up to 60 seconds for VSS,
which can take more than 45 seconds on a busy host; VSS holds writes for at most 10 seconds, so its first thaw can report
that error; the retry succeeds. The request field `quiesce` of the backup call is
absent (automatic: freeze when the agent is connected and can freeze; if that fails, back up without), `true` (freeze or fail the
backup) or `false` (never). Replication copies are automatic; scheduled backups send `false`. The task note says "guest file
systems frozen" or "not frozen: REASON", and `manifest.frozen` is `true` or `false` for a live backup (absent for older backups
and shut-off VMs). Tasks that ended well carry such a note as `note` in `GET /api/tasks`.

`POST /api/tasks/{id}/cancel` asks a running task to stop. It works for backups, replication copies, migrations, disk
moves, image downloads, evacuations, rolling updates, restores of a backup, full clones of a VM or a volume, imports of
a disk or OVA, backup exports and imports, volume downloads from a URL, volume uploads and backup verifies. For the first seven it does
the same as the cancel call of that action. A running task that can be stopped carries `cancel` in `GET /api/tasks`: the
role needed to stop it (`operator`; `admin` for an evacuation and a rolling update). The Tasks panel and the Tasks page
show an **x** on such a task. An evacuation and a rolling update finish the step they are in first.

A task that was stopped ends as failed with a message that contains "cancelled", for example "clone cancelled". A request
that runs such a task and waits for it answers 409 with that message (`{"error":"clone cancelled"}`), and the controller does not log it
as an internal error. The work on the node ends within a few
seconds, and nothing half-made stays: partial volumes, partial files, work directories and half-defined VMs are removed.
The source (the backup, the template, the uploaded file) is never changed. Some steps cannot be interrupted, and the
stop is then seen as soon as the step ends:

| Task | Not interruptible | Point of no return (a later stop does nothing) |
| --- | --- | --- |
| Restore | none | Moving the disks into place and defining the VM (a restore that replaces a VM removes the old one only after this point) |
| Clone | `virt-sysprep`, `virt-customize` | Defining the VM |
| Disk or OVA import | none | OVA: registering the finished volumes |
| Backup import | none | Checking the archive and moving it into place |

A clone of a volume or VM copies through libvirt, which cannot be interrupted. The node command then ends the
`qemu-img` that libvirt started (`copy-kill`). On a node whose node command is older, the copy runs to its end and the
task is stopped afterwards (run `virticle node prepare` again). A linked clone takes no time and cannot be stopped.

### Incremental backups

- Each backup of a running VM with qcow2 disks leaves a libvirt checkpoint (a QEMU dirty bitmap per disk), named
  `virticle-<time>-<random>`, with the target (`node:pool`) as its description. The next backup asks libvirt for the blocks
  changed since the checkpoint of the newest backup that still has one.
- The manifest of an incremental backup is format 2 (an older Virticle refuses it instead of restoring part of a disk) and adds
  `kind`, `parent` (a backup in the same pool) and `checkpoint`. A full backup stays format 1, with `kind`, `checkpoint`,
  `fullReason` and the disks' `source` added; backups from before have none of these and count as full.
- A backup is full when: the VM is shut off (copied with qemu-img, no checkpoint); a disk is not qcow2; no earlier backup of the VM in the target has its checkpoint any more (restart after a killed QEMU, migration,
  restore, snapshot revert, disk move); the disks (target, size, and the base image at the bottom of the backing chain) differ from the parent's; a backup in the chain is
  missing or its files differ from the manifest; the chain already has 7 backups; the VM's schedule keeps the run as a
  weekly or monthly point (see [Retention](#retention)); or libvirt refuses the incremental backup (then the VM's
  checkpoints are dropped and a full backup is made).
- The checkpoints of the same target older than the new one are deleted after a backup; a VM keeps at most 4. A failed or
  cancelled backup deletes the checkpoint it made.
- Retention ([Retention](#retention)) keeps every backup a kept one builds on, and deletes newest first; it stops at
  the first backup it cannot delete. A restore reads the chain with one
  `qemu-img convert` over a `json:` description of the images; nothing reads the VM's disks.
- Snapshot revert, migration (live or offline) and disk move drop the VM's checkpoints first. Taking or deleting a snapshot (internal,
  or external with its overlay merged back) and a resize keep them: libvirt carries the change tracking into an overlay and
  merges it back (tried on the lab: create, two stacked overlays, middle and active merge, with writes during the merge;
  every restore matched). Deleting a backup is refused while a backup of the same VM runs.
- Two limits of libvirt 11.3, both harmless to the data. An external snapshot taken while a checkpoint exists leaves that
  checkpoint's bitmap in the read-only base image, where libvirt cannot remove it; it stays there and QEMU keeps updating it.
  A libvirt restart during a backup leaves a temporary bitmap in the VM; the next backup removes it through the QEMU
  monitor (libvirt marks the VM as tainted in its log) and carries on.
- A controller or node that stops in the middle of a backup leaves its hidden work directory (`.virticle-bk.XXXXXX`) in
  the pool directory. It is removed as described under "Work directories" (Replication). A directory no record names is
  removed by the next backup once nothing in it has changed for an hour, when no other backup of that controller is running.

### Encrypted backups

[age](https://age-encryption.org/v1) with one X25519 key per installation (the backup key). Turned on and off under
**Settings > Backup encryption**; nothing else is configurable.

| Where | What | How long |
| --- | --- | --- |
| Web setting `backup-encryption` (database) | `on`, the public key (`recipient`, `age1...`), when it was made | Until a new key; turning off keeps it |
| `<data dir>/backup-key.age` (mode 0600) | The recovery key file: an armored age file, scrypt-encrypted with the passphrase, holding a standard age identity file | Kept, also when encryption is off; part of every controller backup |
| The passphrase | Only in the request that turns encryption on, changes the passphrase, restores, or checks it for an export | That request; never stored, logged or put into a task or the audit log |
| The identity (`AGE-SECRET-KEY-1...`) | Controller memory, after the passphrase opened the key file | A restore: until its task ends (the bytes are cleared then). An export of a chain: up to 2 minutes, for one download by the same user |
| The identity on a node | stdin of `age-decrypt`, through the SSH session; never a command line or a file | That one verb |

**Files of a backup.** With encryption on, each data file is encrypted on the node that makes it, in the run's work
directory, right after the copy and the qcow2 check, and before a copy to another node: `<target>.qcow2.age`,
`nvram.fd.age`, `tpm2-state.bin.age`. The plain work file is deleted as soon as its encrypted copy is complete (the node
finished, the stored size matches). `manifest.json` and `domain.xml` stay plain, so a backup is listed, checked, pruned and
exported without the passphrase. Replication copies are never encrypted.

**Manifest fields** (the format number does not change):

| Field | Meaning |
| --- | --- |
| `recipient` | The public key the encrypted files are encrypted to. Present exactly when a file is encrypted |
| `disks[].encrypted` | `file` is stored encrypted (named `.age`); `bytes` and `sha256` are those of the stored file |
| `disks[].plainBytes`, `disks[].plainSha256` | Size and SHA-256 of the qcow2 image inside |
| `nvramEncrypted`, `nvramBytes`, `nvramSha256`, `nvramPlainBytes`, `nvramPlainSha256` | The same for `nvram.fd.age` |
| `tpmEncrypted`, `tpmPlainBytes`, `tpmPlainSha256` | The same for `tpm2-state.bin.age`; `tpmBytes` and `tpmSha256` are then the stored file's |

A plain file never ends in `.age` and has no plain fields; an encrypted file needs all four values. Each file says what it is,
so a chain may mix plain and encrypted backups (turned on or off between two backups). An older Virticle lists such a backup
and fails on the encrypted file when it restores it.

**Restore and export.** A restore whose chain holds an encrypted file needs `passphrase` in the request body (`POST
.../restore`). A wrong passphrase is refused (400, "wrong passphrase for the backup key") before anything is staged, and
counts like a wrong password (locked for a while after 5). A backup encrypted to another key is refused with that key named.
The node that holds the backup decrypts (`age-decrypt`) into the restore's work directory, checking the stored and the plain
SHA-256 in the same pass; then the restore goes on as for a plain backup. A single backup restored on its own node is
decrypted straight into place. The audit log says "restore of an encrypted backup".

`GET .../export` of a full backup sends the stored files: encrypted files stay encrypted and the manifest says so. An
incremental backup whose chain holds an encrypted file is put together on export, which needs the key: `POST
.../export-key {passphrase}` answers `{ticket}`, and `GET .../export?ticket=T` downloads once, within 2 minutes, for the same
user. The put-together disks are encrypted again to the same key. Import takes both kinds; an encrypted file is checked when
it is restored.

**API.**

| Call | Role | Does |
| --- | --- | --- |
| `GET /api/backup-encryption` | viewer | `{on, hasKey, recipient, since}`; with `?count=1` also `vmBackups` and `controllerBackups` that need the key |
| `POST /api/backup-encryption/on` | admin | `{passphrase, again}` makes the key the first time and answers the recovery key file in `keyFile`; with a key, `{passphrase}` of that key |
| `POST /api/backup-encryption/off` | admin | New backups are plain again; the key stays |
| `POST /api/backup-encryption/passphrase` | admin | `{current, new, again}`: the same key under a new passphrase (`keyFile` in the answer) |
| `POST /api/backup-encryption/key` | admin | `{password, code}` (web session only): downloads `virticle-backup-key.age` |

**Node verbs** (protocol 21; an older node command makes a backup fail while encryption is on, never store it plain):
`age-encrypt SRC DST RECIPIENT` (SRC and DST in one work directory) and `age-decrypt SRC DST` (identity on stdin, DST in a
work directory). Both stream, print `@@progress DONE TOTAL` once a second and end with both checksums and sizes. A stop
ends them; DST appears only complete.

**Decrypt by hand** with the stock `age` tool (1.1 or newer opens a passphrase-protected key file; it asks for the passphrase):

    age -d -i virticle-backup-key.age -o vda.qcow2 vda.qcow2.age
    qemu-img check vda.qcow2
    sha256sum vda.qcow2        # equals disks[].plainSha256 of the manifest

The same works for `nvram.fd.age`, `tpm2-state.bin.age` and a scheduled controller backup
(`age -d -i virticle-backup-key.age FILE.tar.gz.age | tar tz`). Verified with age 1.3.2 on a Rocky 10 node.

**Cost.** The backup job and qemu-img write qcow2, which needs a seekable file, so encryption is a second pass. It replaces
the checksum pass: the node takes both checksums while it encrypts, in parallel. Lab (nested Rocky 10, 2 vCPUs, a backup of
1.9 GB): a shut-off VM took 14.5 to 18.9 s plain and 19.5 to 24.0 s encrypted; the work directory peaked at 4.1 GB instead
of 2.0 GB (one more copy of the largest disk, for a moment). The free-space check counts it.

**Limits.** Encryption protects backups at rest: a copied pool, a stolen disk, an export file, the controller's scheduled
copies. It does not protect against someone who controls a node, where the VM's own disks are plain, or the controller
while a restore runs. Losing the passphrase or every copy of the recovery key file loses the encrypted backups. There is no
way to delete or replace the key in this version.

### Retention

One rule set for scheduled VM backups (per VM and target node and pool), for a manual backup into the schedule's
target, for the controller's copies on its server and for `virticle backup -o DIR --keep-*`. The same code prunes and
computes the preview, so the two agree. Walking the backups newest first:

| Field (API) | Flag | Keeps |
| --- | --- | --- |
| `keepLast` | `--keep-last` | the newest N |
| `keepHourly` | `--keep-hourly` | the newest backup of each of the last N distinct hours that have a backup |
| `keepDaily` | `--keep-daily` | the same per calendar day |
| `keepWeekly` | `--keep-weekly` | the same per ISO week (Monday to Sunday) |
| `keepMonthly` | `--keep-monthly` | the same per calendar month |

- A backup kept by any rule is kept; the rest is deleted. Every count is 0 to 1000. All 0 keeps everything (a manual
  backup's `keepLast: 0`); a schedule must keep something.
- Periods are counted in the controller's local time zone, from the creation time in the manifest (VM backups) or the
  file name (controller backups). The two 01:00 hours of a night when clocks go back are two hours.
- Example, backups at 03:00 every day from 1 May, now 14 Nov, `keepDaily 7, keepWeekly 4, keepMonthly 6`: the dailies
  8 to 14 Nov; weeklies 14 Nov (also a daily), 8 Nov (also a daily), 1 Nov and 25 Oct; monthlies 14 Nov, 31 Oct, 30 Sep,
  31 Aug, 31 Jul and 30 Jun. That is 14 backups back to 30 Jun; incremental ones keep their chains on top of that.
- A rule that cannot have an effect at the schedule's frequency is stored as 0: hourly for a schedule that runs at most
  once a day, daily for at most once a week, weekly for at most every four weeks.
- Never deleted: the backup just written (only after it reads back complete), a backup a restore or an export reads at
  that moment, and every backup they build on. Deleting such a backup by hand is refused.
- The task of a run that deleted backups ends with the note "N older backups deleted (keep RULES)".

**Full weekly and monthly points.** `retention.PointFull` decides, for the scheduler and the preview alike, which run
of a VM schedule with `keepWeekly` or `keepMonthly` is made full:

- The rules keep the newest backup of each week and month, so the point of a period is its last run. A run is the last
  one when the schedule's next run (from the schedule's timing, as if this backup were the newest) falls into a later
  week or month. A next run due less than 2 minutes (at most half a step) before the period ends counts as later,
  since the scheduler may start it that late.
- Such a run is made full (reason "Weekly or monthly point") only when the newest
  backup before it is in the same period. A run alone in its period (a weekly schedule) builds on the point before it,
  which is kept anyway.
- A manual backup into the schedule's target is decided the same way: the schedule times its next run from the newest
  backup. A paused schedule decides nothing.
- A missed last run (controller down) leaves the period's point incremental, with its chain. Existing schedules need no
  change; the rule applies from their next run on.
- Daily and hourly points stay incremental and keep their chains. The 7-backup rotation counts from the last full
  backup, whatever made it full.

**Metadata.** The `backup-schedule` element keeps `keep-last` for a schedule with keep-last only, written as before.
A schedule with any other rule is written with `keep-last="0"` and `keep-newest`, `keep-hourly`, `keep-daily`,
`keep-weekly`, `keep-monthly`: a controller from before the rules refuses it (its keep-last must be 1 or more) instead of
pruning it down to keep-last alone.

**API.**

- `GET/PUT /api/nodes/{id}/vms/{uuid}/backup-schedule`: the schedule has `keepLast`, `keepHourly`, `keepDaily`,
  `keepWeekly`, `keepMonthly` (a field left out of a PUT is 0, `keepLast` 7).
- `POST /api/nodes/{id}/vms/{uuid}/backup` with `keepLast`: into the pool of the VM's schedule (same node and pool) the
  schedule's rules apply instead of `keepLast`.
- `POST /api/nodes/{id}/vms/{uuid}/backup-schedule/preview` (operator) takes an unsaved schedule (the PUT body) and
  answers what it keeps. `POST /api/controller-backup/preview` (admin) does the same for the controller's schedule.
  The answer: `policy` (the rules as they would be stored), `ineffective` (rule: reason), `nextRuns` (Unix seconds),
  `points` (the backups of the steady state, newest first: `time`, `reason`, `rules`, `full`), `kept`, `oldest`,
  `complete` (false when very frequent runs were cut short at 600 000 simulated runs), `unbounded`, `zone`, `chains`,
  `size` (`low`, `high`, `basis` `backups`/`disks`/`last`, `full`, `incremental`, `measured`, `fulls`, `incrementals`),
  `free` and `used` (bytes in the target, and what the VM's backups or the controller's files take there now).
- The steady state is the schedule run on paper from far enough back to fill the longest rule, up to now, pruned with
  the same rules (chains: a full backup, then up to 6 incremental ones, and a full one for each weekly or monthly
  point, for a running VM with qcow2 disks). The size is the
  number of full backups times the size of the VM's last full one in the target, plus the incremental ones times the
  average of the recent ones, plus or minus 20%; without an incremental one measured, 2 to 20% of a full backup each;
  with no backup at all, the allocated size of the disks.
- `GET/PUT /api/controller-backup`: `daily` (on), `mode` (`daily` or `interval`), `at` (HH:MM, server time),
  `everyHours` (1 to 168), the five `keep*` fields, `dir` (the folder of the scheduled copies; empty or the default
  `backups` in the data directory), `zone`, `nextRun`. Fields left out of a PUT keep their values. A new `dir` must be a
  full path outside the data directory; the controller creates it and writes a test file there before it saves, and
  refuses it with the reason otherwise.
  Stored settings from before have only `daily`: daily at 03:30 with `keepDaily 7`. The old field `keep` is gone.

## Node access

A node is reached in one of two ways. Both run the same program as root: the node command
(`/usr/local/libexec/virticle-node`), which `virticle node prepare` installs. See [install-node.md](install-node.md).

| Access | How to set it up | What the controller does |
| --- | --- | --- |
| **Service account** (recommended) | `virticle node prepare` | Signs in as `virticle-node` (`virticle` on a node that 0.2.0 prepared) and runs the node command with `sudo -n`. The sudo rule allows that program and nothing else. |
| **Root** | `virticle node prepare --root` | Signs in as root and runs the node command directly. A Service account is safer. |

**Minimum versions.** A node needs libvirt 11.0 or newer and QEMU 10.0 or newer (the numbers are in
`internal/minver`; there is no setting to change them). The check is made in three places, with the same sentence: `node
prepare` and the Add node dialog (installed versions, or the versions the repositories would install, before anything is
installed), the connection test, and every connection the controller makes to a node. A node below the minimum is not
managed: its state is Offline, its error is "This node has libvirt 10.0.0 and QEMU 8.2.2. Virticle needs libvirt 11.0 and
QEMU 10.0 or newer.", and every action on it is refused as for any offline node. The controller connects again after five
minutes, so an upgraded node comes back by itself; saving the node's connection settings does it at once.

A node that has neither is "Not prepared": the features that need root are off. A node whose account has general
passwordless sudo but no node command is also not prepared.

### Connection

One SSH connection per node (through the jump host when one is set), with libvirt on top. A node that hangs, is frozen or
loses power sends nothing, so its TCP connection stays open; these limits make sure the controller notices and connects
again by itself:

| What | Limit |
| --- | --- |
| TCP connect, SSH handshake (also through the jump host) | 15 s each |
| libvirt handshake | 30 s |
| Everything else until the node is online (host name, access check, events) | 60 s |
| SSH keepalive to the node and to the jump host | every 15 s; no answer within 30 s ends the connection |
| libvirt watchdog: a trivial call | every 30 s; no answer within 15 s ends the connection |

After a lost connection the controller tries again after 1 s, then waits twice as long each time, up to about 30 s.

## Guest setup on clone

The clone dialog's **Guest setup** section (user, password, SSH keys, packages) reaches the clone in one of these ways.
The controller picks it; there is no option.

| Guest of the source | How | Notes |
| --- | --- | --- |
| Windows (by the VM's OS entry or a Windows seal) | Answer file on a disc, read by Windows setup | As before: computer name, administrator, password only. |
| Linux with cloud-init | NoCloud seed disc, read by cloud-init at first start | As before. |
| Linux without cloud-init (or with `/etc/cloud/cloud-init.disabled`), Debian, Ubuntu, RHEL, CentOS, Fedora, Rocky, Alma, Oracle, openSUSE or SLES family, guest disk tools on the node | Written into the clone's disks while it is shut off, after the new identity step | See below. Profiles and raw user-data are refused: they need cloud-init. |
| Anything else: another Linux (NixOS, Alpine, CirrOS), no system found, an encrypted disk, Windows on a VM not defined as Windows, `virt-customize` missing | Not possible | The dialog says "Not possible: REASON"; a clone request with these settings is refused with the same reason. |
| Node without the guest disk tools (`prepare --minimal`) | Seed disc, not checked | The dialog says so: it needs cloud-init in the guest. |

**Detection.** The node command (`guest-inspect`) starts one read-only `guestfish` appliance on the source's disks,
runs the libguestfs inspection (system type, distribution, version, package manager) and looks for
`/usr/bin/cloud-init`, `/etc/cloud/cloud-init.disabled`, `useradd`, `sudo`, `qemu-ga` and `/etc/selinux/config`.
About 10 seconds on a small nested lab node. It runs when the section is first opened in the dialog. The answer is
kept in the controller's memory: for a template as long as the size and allocation of its disks stay the same, for
another shut-off VM 10 minutes. A running VM is not looked into.

**Written into the disk** (`guest-customize`, `virt-customize --no-network`):

- Host name: the clone's name (`--hostname`).
- User: created when missing (`useradd -m`, shell bash, the `sudo` group or else `wheel`, and
  `/etc/sudoers.d/90-virticle-USER` with `NOPASSWD:ALL`, as the seed disc gives). An existing user keeps its groups and
  gets only the password and keys.
- Password: hashed by the controller (SHA-512 crypt, as for the seed disc); the hash goes to the node on standard input
  and into the guest script only. With a password, `/etc/ssh/sshd_config.d/00-virticle-password.conf` allows password
  login, as the seed disc does.
- SSH keys: appended to `~/.ssh/authorized_keys` (directory 700, file 600, owned by the user). On a guest with SELinux
  the files are relabelled (`--selinux-relabel` for older virt-customize; newer ones relabel by themselves).
- Packages: installed at the first start (a `--firstboot` script of the node command), by the guest's package manager
  (`apt-get update` and `apt-get install`, `dnf`, `yum` or `zypper`) over the guest's own network, like cloud-init does.
  The script tries again every 10 seconds for 5 minutes, because the first-boot service starts before the network has
  an address. The guest agent is added when the guest has none, and `sudo` when it is missing and a
  user is given. Not at clone time, because that would need network access from the node's appliance and make the
  clone slow; a failed install shows only in the guest (`/root/virt-sysprep-firstboot.log`).

The node command builds the guest script itself from a checked request (user name, hash, keys, package names and host
name, each with a fixed pattern); the caller cannot pass commands. The script is in a private directory under
`/run/virticle-node` while `virt-customize` runs and is removed afterwards.

## Versions and the node command

- The node command protocol is **22** (0.4.0 had 21, 0.3.x had 20, 0.2.0 had 10). A node whose node command has another number shows
  "prepared by an older version" (**Outdated** on the Nodes page) and must be prepared again
  ([install.md](install.md#upgrading)). The old node command would also refuse the new requests, because it rejects fields
  it does not know.
- Verbs new since 0.4.0: `guest-inspect`, `guest-customize` ([Guest setup on clone](#guest-setup-on-clone)).
- Verbs new since 0.3.x: `age-encrypt`, `age-decrypt` (encrypted backups), `copy-kill` (stopping a clone), and `sha256 PATH
  progress` (verify).
- Verbs new in 0.3.0, by feature (all described in [install-node.md](install-node.md#verbs)): virtual switches
  (`net-info`, `net-plan`, `net-apply`, `net-run`, `net-confirm`, `net-rollback`, `net-status`, `net-boot`); backups
  (`backup-detach`, `df`, `work-stale`, and `convert-start` taking the incremental images); firmware (`nvram-new`,
  `disk-boot`); rolling updates (`update-start`, `update-run`, `update-status`, `node-reboot`, `selfcheck`); replication
  (`repl-overlay`, `repl-rebase`, `repl-commit`, `repl-ls`, `mtime`, `qcow2-bitmap-rm`, `work-ls`, `tpm-save`, `tpm-place`,
  `hook-qemu`, `peer-ask`, `vm-probe`).
- The database schema is **6**, the same as in 0.3.x (0.3.0 added the table `rolling_runs`). A controller that finds a database of a newer schema
  logs a warning and carries on; do not use that, see below.
- **Two controllers of different versions must never manage the same nodes.** An older controller does not know the
  incremental backup chains (its retention deletes backups that a newer chain needs), the replica and old-copy marks in the
  VMs, or a rolling update that is running. When you upgrade, stop the old controller first. To try a new version beside
  the old one, give it nodes of its own.

## Host bridges (virtual switches)

A node's Linux bridges can be created, changed and deleted from the controller (admin only, API
`/api/nodes/{node}/hostnet`; `GET .../hostnet/peers?bridge=&network=&vlan=` lists the other nodes that have a managed bridge of that name and no network of that name, for the UI's "create the same network" box). Only Linux bridges are handled; Open vSwitch is not. The change touches the network the
controller reaches the node over, so it is built to undo itself.

**What a change does.** The node command first saves the current settings (the configuration files it will touch, the
NetworkManager profile of the uplink, the running addresses and routes) in `/var/lib/virticle-node/net/`, readable by
root only. Then it arms a rollback timer (a transient systemd timer, independent of the SSH session), and only then
changes the network, in a transient systemd unit of its own, so it completes even when the SSH connection dies.
"Create a bridge with an uplink and move the addresses" gives the bridge the uplink's addresses, gateway, DNS and MTU, and
its MAC address (a DHCP lease follows the MAC address, so the node keeps its address; with systemd-networkd the bridge also takes over the card's DHCP client identifier, otherwise a server that keys its leases by client id hands out another address), and leaves the uplink as a plain
port. The node keeps the address the controller has stored, so the controller reconnects by itself.

**If the node becomes unreachable.** The controller waits for the node to come back, checks that libvirt answers, and
confirms the change; the node then keeps it. If it does not confirm within **90 seconds** (adjustable from 30 to 600), the
node restores its previous settings by itself and the task is recorded as failed ("The node did not come back in time. It
restores its previous network settings by itself; check again in a minute."). When the node is reachable again, its status
(`GET .../hostnet`, `recent`) says that the change was rolled back and why (`timer`, `apply failed`, `check failed`). A
change that cannot even apply (a command fails, the bridge does not get its address in 40 seconds) is rolled back at once,
without waiting for the timer. Only one change can be pending per node. Changes are logged to the journal with their id
(`journalctl -t virticle-node`); the last five are kept under `/var/lib/virticle-node/net/changes/`.

**From the node's console.** If neither the apply nor the timer got the node back, log in at the console as root and run

    /usr/local/libexec/virticle-node net-rollback

which restores the pending change (or give the id from `virticle-node net-status`). It is safe to run twice. A change that
was already confirmed is permanent: undo it by deleting the bridge.

**If the node restarts before the confirmation.** The rollback timer is a transient unit and is gone after a reboot. The
first change installs a small persistent unit, `virticle-net-boot.service` (it does nothing unless a change is pending),
which runs `virticle-node net-boot` at boot: the change gets its grace period again, counted from the boot (the same
number of seconds it had, 30 to 600), and is rolled back when nobody confirms it. A node therefore never stays on an
unconfirmed configuration for good. `virticle node unprepare` removes the unit with the node command.

**Several uplinks (a bond).** A switch can have two or more physical cards. They become the members of a Linux bond
(`vswbond0` for the switch `vsw0`, otherwise `b-` and the switch name, 15 characters at most) and the bond is the bridge's only
port. Two modes are offered: `failover` is `active-backup` with `miimon 100`; `lacp` is `802.3ad` with `lacp_rate fast`,
`xmit_hash_policy layer2+3` and `miimon 100`. `balance-alb` and `balance-tlb` are not offered: they rewrite MAC addresses of the
frames and the cards' ARP replies, which breaks a bridge that carries MAC addresses of virtual machines. Changing the cards
(`set-uplinks`): a card is added to the bond, a card is released (it gets its own configuration back), a one-card switch becomes
a bond, the mode changes (the members are released and the bond is set again in place; the bond is down for a moment). A bond
whose cards are removed down to one stays a bond with one member (no second change of the bridge port is needed); the switch
is only collapsed by deleting it. The card whose addresses moved to the bridge cannot be removed. A card that joins with an address of its own
loses it (the plan says so) and gets it back when it leaves. The bond and the bridge both get the MAC address of the card whose
configuration moved, and all members take the bond's MAC address (active-backup), so a DHCP server sees one MAC address and keeps the lease;
the VLAN ids go on the bond. Written: networkd, `05-virticle-BOND.netdev` (`Kind=bond`, `[Bond]` Mode, `MIIMonitorSec=100ms`,
`LACPTransmitRate`, `TransmitHashPolicy`), `05-virticle-BOND.network` (`Bridge=` and the `[BridgeVLAN]` entries) and one
`05-virticle-BRIDGE-CARD.network` per card (`Bond=`); NetworkManager, `virticle-bond-BOND` (`bond.options`, port of the bridge,
`bridge-port.vlans`) and one `virticle-port-CARD` per card (`controller BOND`, `port-type bond`). Rollback and delete restore
each member's own configuration. Status (shown in the Network tab): `/proc/net/bonding/BOND` gives each member's link, the active
member (failover) and, for LACP, the partner MAC address: all zeros means the physical switch did not answer. LACP needs
the cards to report their speed; the plan warns when they do not or when the speeds differ.

**The node's address on a VLAN.** On a managed, VLAN-aware switch the node gets an interface `SWITCH.ID` (a VLAN device on the
bridge, 15 characters at most) with DHCP or a static IPv4 address. The bridge becomes a member of the VLAN itself
(`bridge vlan add dev BRIDGE vid ID self`) and the port carries the id tagged: the id is added to the switch's allowed list when needed
(the plan says so). Without the default route option the address adds no default route (static: no gateway; DHCP: gateway,
DNS, NTP and search domains of the lease are ignored). Written: networkd, `05-virticle-SWITCH.ID.netdev` (`Kind=vlan`) and `.network`,
`VLAN=` and `[BridgeVLAN]` in the bridge's `.network`; NetworkManager, `virticle-vlan-SWITCH.ID` (`vlan.parent`, `vlan.id`) and `bridge.vlans` on
the bridge. The address interface is created without touching the bridge's own configuration. Removing an address that the controller uses is
refused, and so is changing the VLAN list or deleting the switch in a way that cuts such an address. Deleting the switch removes its
VLAN addresses. Moving the node's main (management) address onto a VLAN is not supported: the controller would have to reach the
node at a new address through a path that this change cannot check beforehand.

**MTU.** `set-mtu` (and the MTU field when a switch is added) sets one value on every device of the switch: each card,
the bond, the bridge and the VLAN interfaces (networkd `MTUBytes=` in the `.netdev` or `[Link]` of each file; NetworkManager `802-3-ethernet.mtu` of each profile), and
applies it at once with `ip link set` (cards last when it goes down, first when it goes up). When a card leaves the switch, or the
switch is deleted, the card gets its earlier MTU back. libvirt 11.3 and 11.10 refuse `<mtu>` in a network with `forward mode='bridge'`, so
networks on a host bridge cannot carry it; a guest needs the MTU in its own settings (not tried in the lab).

**What it refuses.** Ports of a bond (or of a bridge) as uplinks, and cards that are already in another bond; an uplink that is already a port; wireless, virtual and
libvirt-managed (`virbr*`, `vnet*`) interfaces; changing an interface that carries the controller's connection without
moving its addresses; a second change while one is pending; deleting a bridge that VMs or libvirt networks use; changing
or deleting a bridge set up outside virticle whose saved definition has a setting virticle does not have (below).
virticle keeps a record of the bridges it manages (`/var/lib/virticle-node/net/managed/`).

**Bridges set up outside virticle.** A Linux bridge without a record is read from its saved definition. When every setting of
that definition is one virticle has (or that setting's default), the bridge is a virtual switch like one virticle created: the
same actions, no label. The first change to it takes the definition over, under the same rollback timer, and after the
confirmation the bridge has a record like any other. Otherwise the bridge is listed, can carry networks and VM interfaces, and
`notEditable` (a few words) and `notEditableDetail` (a sentence) in `GET .../hostnet` say why it cannot be changed. The check is an
allow-list per configuration system: a key it does not list makes the bridge not editable.

| Setting | Accepted (NetworkManager keyfile / netplan YAML) |
| --- | --- |
| Where it is saved | NetworkManager: keyfiles in `/etc/NetworkManager/system-connections`, one profile each for the bridge, its port (one card or one bond) and each bond member, all active now and started by themselves, bound by interface name. netplan: one entry each for the bridge, its port and the bond's cards, each in one file in `/etc/netplan` (not `/lib` or `/run`), renderer networkd, and netplan's generated files in use |
| STP | off (`bridge.stp=false`; netplan `parameters: {stp: false}`). Both default to on |
| Other bridge options (forward delay, priority, multicast, ageing, hairpin, path cost) | not set |
| Ports | at most one: a physical card, or a bond of physical cards; VM interfaces (`vnet*`, `tap*`) besides |
| Bond | `mode=active-backup,miimon=100`, or `mode=802.3ad,miimon=100,lacp_rate=fast,xmit_hash_policy=layer2+3` (netplan: the same as `parameters`, `mii-monitor-interval: 100`), nothing else |
| Addresses | NetworkManager: the `ipv4`/`ipv6` keys virticle copies from a card (method, addresses, gateway, DNS, routes, metrics, DHCP options); netplan: `addresses` (plain strings), `dhcp4`, `dhcp6`, `routes`, `nameservers`, `gateway4/6`, `link-local`, `accept-ra`, `optional`, `critical`, `dhcp4/6-overrides`, `ipv6-privacy`, `dhcp-identifier` |
| MAC address | only on a bridge with addresses, and the one it has now |
| MTU | none, or the same value on the bridge, the bond and every card |
| VLANs | NetworkManager: `vlan-filtering`, `vlan-default-pvid=1`, port `vlans` with `1 pvid untagged` and plain tagged ids. netplan has no VLAN filtering: a bridge that filters VLANs is refused. VLAN interfaces on the bridge, the bond or a card are refused |
| Ports' own settings | NetworkManager: `ethernet.mtu`, `mac-address` of the card, empty `bond-port`. netplan: `match` (MAC address or name of the card), `set-name`, `dhcp4: false`, `dhcp6: false`, `link-local: []`, `accept-ra: false`, `optional`, `mtu` |
| cloud-init | a bridge in `50-cloud-init.yaml` is refused while cloud-init is installed and its network configuration is not disabled (it writes the file again at boot) |
| Not editable at all | `virbr*` and bridges of libvirt NAT, routed, isolated and open networks; container bridges (`docker*`, `br-` and 12 hex digits, `podman*`, `cni*`, `lxcbr*`, `lxdbr*`, `incusbr*`); Open vSwitch; a bridge made with `ip link` (no saved definition); plain systemd-networkd files of the administrator and ifupdown (not edited yet) |

How the definition is taken over:

- NetworkManager: virticle's profiles (`virticle-BRIDGE`, `virticle-bond-BOND`, `virticle-port-CARD`) are added with the same
  settings and activated on the existing devices, which NetworkManager does without removing the bridge; then the original
  profiles are deleted. Their keyfiles are saved with the change; a rollback writes them back, reloads, activates them and
  deletes virticle's profiles, so the profiles, their UUIDs and the files are as before. When the bridge has addresses, the first
  card gets a profile of its own (named like the card, or `virticle-card-CARD` when that name is taken) with the bridge's address
  settings and autoconnect off: the card gets it back when the switch is deleted.
- netplan: the bridge's entry (and the bond's) is removed from the YAML; when the bridge has addresses, its address settings
  move to the first card's entry, which is what the card gets back when the switch is deleted. Only those lines change; comments and
  other entries stay. virticle's networkd files take the bridge's place and `netplan generate` runs (not `netplan apply`, which would
  remove the bridge). A later `netplan apply` or the `netplan generate` at boot find one definition of the bridge: virticle's. The YAML
  files are saved with the change; a rollback writes them back byte for byte and generates again.
- Deleting a recognised bridge without a change before: NetworkManager deletes the profiles and gives the first card a profile with
  the bridge's address settings; netplan removes the entry and moves the address settings to the card's entry. The review lists the
  profiles and files by name.

**VLANs.** A VLAN-aware bridge has VLAN filtering on; its uplink allows the chosen VLAN ids (2-4094 by default, tagged),
and the bridge's own address stays untagged (VLAN 1). A libvirt network with `forward mode='bridge'` on such a bridge can
carry a VLAN tag (`vlan` in the network request): the tag must be one the uplink allows.
The node's own addresses on a VLAN are described below.

**Configuration systems.** Detected at run time, never by distribution name:

| System | Written | State |
|---|---|---|
| NetworkManager | connections `virticle-BRIDGE` (bridge), `virticle-port-UPLINK` (and `virticle-bond-BOND`, `virticle-vlan-IF`); the uplink's own connection is kept and only set not to start by itself; the profiles of a bridge set up outside virticle are replaced when it is taken over (above) | tried on a real node (Rocky Linux 10, NetworkManager 1.56): create, move the management address, VLANs, rollback by timer and by hand, delete, reboots |
| netplan with systemd-networkd, or plain systemd-networkd | `05-virticle-*.netdev` and `.network` files in `/etc/systemd/network`; the files of netplan and the administrator are not edited, except the netplan YAML of a bridge set up outside virticle that is taken over (above). They sort before netplan's `10-netplan-*.network`, survive a reboot, `netplan apply` and cloud-init | netplan with networkd tried on a real node (Debian 13): the same tests as above; plain networkd without netplan runs the same code but was not tried. To tell which file configures a card, the node evaluates the `[Match]` keys `Name`, `OriginalName`, `MACAddress`, `PermanentMACAddress`, `Kind`, `Type`, `Virtualization` and, for non-wireless cards, `WLANInterfaceType`, which covers the files in `/usr/lib/systemd/network` on Debian 13 and Ubuntu 24.04. A file with another key (for example `Driver` or `Path`) that sorts before the card's own file stops the review with a message that says to give the card a file of its own |
| ifupdown (`/etc/network/interfaces`) | nothing: bridges are refused with a plain message | not supported |

## Shared storage (NFS)

| Call | Meaning |
|---|---|
| `GET /api/storage/shared` | NFS pools grouped by name: nodes that have it, nodes missing it, nodes that have a pool of that name for another server or export, state, size. A server given by name on one node and by address on another is one share. |
| `POST /api/storage/shared` | `{name, server, path, nodes?}` adds a netfs pool on every online node (or the listed ones). Answers `complete`, `message`, `note` (a sentence about a mount that works but has a catch: NFS version 3), per-node results with the NFS `version`; repeatable. |
| `DELETE /api/storage/shared/{name}` | Unmounts and forgets it on all nodes; refused while a defined VM has a file below the mount, or a node is unreachable. |

Facts

- The pool is a libvirt `netfs` pool named NAME, mounted at `/mnt/NAME` on every node, set to autostart, with
  `<format type='auto'/>` (plain `mount -t nfs`; hard mount; highest version offered). Add creates and removes a
  small test volume to check that a disk made by root can be used by the VM user.
- A node that boots while the server is away has the pool stopped; the controller starts a stopped autostart NFS
  pool when the server answers on port 2049 and the node has been up for less than an hour.
- `node prepare` writes `/etc/systemd/system/virtqemud.service.d/10-virticle-storage.conf` (Wants and After
  `virtstoraged.service`) on nodes with separate libvirt daemons, so the NFS pools are mounted before VMs
  autostart. Without it a VM on NFS fails to autostart with "no such file".
- Pool state `unavailable` means the mount did not answer `stat -f` within 4 s, or is not an NFS mount. The
  answer comes from a probe (an SSH command with `timeout -s KILL`) that the controller repeats every few seconds
  for nodes that have NFS pools; pages use the last answer and never wait for it. Libvirt is not asked to read or
  refresh a pool that is unavailable. Statistics of VMs with a disk below an unavailable mount are not collected
  (QEMU does not answer the monitor while it waits for its disk), and the VM detail leaves out the disk figures.
- Storage calls to one node are limited to three at a time and the short ones to 6 seconds (the libvirt proxy
  daemon serves the controller's connection with five calls at once; the others stay free for VMs and networks).
  When three are stuck, further storage calls fail at once with "the storage service of this node is not
  answering" until one returns.
- Alert kind `storage-unavailable`, one per node and pool, critical, debounced like the network alerts (raised on
  the second check in a row, about two minutes; cleared on the second check that finds the share answering).
- Migration: a file is shared when source and target have netfs pools with the same export and mount path and
  the same server (by name or by address); it is not copied, not mirrored and not deleted from the source.
  Internal snapshots do not block a live migration when every disk is shared. Disk move, clone and delete
  refuse a VM with a disk on an unavailable share.
- Backups: a backup stored on a shared pool is listed once. Restoring into an NFS pool works (the node command
  falls back to link-and-unlink where the file system has no `RENAME_NOREPLACE`).
- Deleting a volume libvirt cannot remove (it removes files on NFS as the file's owner) falls back to the node
  command, as root.

## Rolling updates

Planning is `internal/api/rollplan.go`, the run `internal/api/rolling.go`. The state of a run is stored in the table
`rolling_runs` (schema 6) after every step; a controller that starts and finds a run in state `running` carries on with it
(also after its own machine restarted). A step in progress when the controller stopped runs again: every step looks at what is
true first (where the VM is, whether the update ran, whether the boot id changed).

| Request | Role | What it does |
| --- | --- | --- |
| `POST /api/rolling-update/plan` `{"nodes":[ids]}` | admin | The plan: per node `updates`, `reboot`, `moves`, `stuck` (VMs that cannot move, with reasons), `action` (`update`, `skip`, `current`); `refusals`; `needsChoice` |
| `POST /api/rolling-update` `{"nodes":[ids],"unmovable":"shutdown"\|"skip"}` | admin | Plans again, refuses with 409 and the first refusal, else starts one task `rolling-update` |
| `GET /api/rolling-update` | viewer | `{run, active}`: the newest run with its nodes, steps (`pending running ok skipped failed`) and, when stopped, `condition` |
| `POST /api/rolling-update/cancel` | admin | Stop after the current step |
| `POST /api/rolling-update/continue`, `/restore`, `/dismiss` | admin | Run the failed step again and go on; put things back; hide the alert |

Order: ids ascending, the controller's machine (an address that is this machine) last. Steps per node: maintenance, evacuate,
stop-vms, update, reboot, wait, verify, maintenance-off, start-vms, return. The restart is needed when the system says so
(`needs-restarting -r`, `/var/run/reboot-required`), a newer kernel is installed, or the update contained a kernel, firmware,
glibc, systemd, dbus, libvirt or QEMU package. Waits: restart 12 minutes, package tool 100 minutes, VM shutdown 3 minutes.
Alert kind `rolling-update`: critical after a failure, warning after a stop, until continued, put back or dismissed.

Node command verbs: `update-start`, `update-status` (JSON), `update-run` (internal, runs in the unit
`virticle-update`), `node-reboot` (restart in 5 seconds, unit `virticle-reboot`), `selfcheck [repair]` (lines `ok|fixed|problem`, tab, id, tab, text).
The update log is `/var/lib/virticle-update/update.log` on the node. dnf: `check-update --refresh`, then `upgrade -y`. apt:
`update`, then `apt-get -y --with-new-pkgs -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade` with
`DEBIAN_FRONTEND=noninteractive`; a source that fails to refresh fails the run.

## Known limits

- Nodes need libvirt 11.0 or newer and QEMU 10.0 or newer from the distribution's repositories. A node below that is
  refused when it is added and is not managed when it is already added (see [Node access](#node-access)).
- A Debian node and a Rocky Linux node cannot migrate VMs to each other
  (emulator path and machine type differ).
- Snapshots are not part of a backup. TPM state is, for an emulated TPM 2.0 only.
- Only file-based, non-shareable disks can be backed up.
- Internal snapshots pause the guest while memory is saved and block live
  migration (not when every disk is on shared storage).
- A VM keeps one kind of snapshot until all its snapshots are deleted (libvirt cannot delete a tree that mixes internal and external ones). On a node that confines QEMU with AppArmor (Debian) external snapshots cannot be reliably deleted, so a running VM there can only take snapshots that save the running state; a live snapshot without it is always external.
- A UEFI VM made before qcow2 variable stores keeps a raw one: its snapshots are external, and reverting a snapshot does not bring back the UEFI variables of that moment (internal snapshots of a qcow2 store do).
- No automatic failover and no fencing: a failover to a replica is started by a person ([backlog-ha.md](backlog-ha.md)). Nothing starts the VMs of a dead node on shared storage ([backlog-storage.md](backlog-storage.md)). Shared storage means NFS only.
- A bridge set up by hand stays not editable when it has STP on, is defined in cloud-init's netplan file
  (`50-cloud-init.yaml`) while cloud-init manages the network, or has any setting outside the switch model
  ([Host bridges](#host-bridges-virtual-switches)). It can still carry networks and VM interfaces.
- Guest setup on clone written into the disk was not tried on SUSE, Ubuntu or a guest installed from a real ISO. NixOS
  and other systems are refused; Windows goes its own way (an answer file).
  Packages are installed at the clone's first start; a failed install shows only inside the guest.
- The clock sync after a start (guests with a local-time hardware clock) needs the guest agent, and does not cover VMs the
  node starts at boot.
- Not verified end to end: ACME issuance, arm64. See [ROADMAP.md](../ROADMAP.md).
