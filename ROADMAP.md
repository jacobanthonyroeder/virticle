# Objectives

A web front end for KVM/QEMU/libvirt that stays fully compatible with `virsh`.
One Go binary, no service on the nodes: everything goes over SSH to each node's
libvirt socket, and the few things that need root run through one small program,
the node command (`virticle-node`), which the controller starts on demand.
Libvirt is the source of truth; the app keeps only users, the node list and task history.

Status: `[x]` built and verified against the lab, `[~]` built with open points, `[ ]` open. What is planned comes
last, under [Planned](#planned).

## Foundation
- [x] Spike: libvirt RPC over SSH, events, upload, live migration with disk copy, live snapshot, snapshot-preserving migration (`cmd/spike`)
- [x] Server binary with embedded web UI, SQLite state, local login
- [x] Node add/remove, persistent connections with reconnect. Every dial step has a time limit and SSH keepalives run to the node and the jump host, so a node that hangs comes back by itself: on the lab a frozen node (suspend) was noticed in 30 to 45 s and was online again within 25 s of a reset, a restart or a resume, 17 of 17 times, also after 10 minutes frozen
- [x] Live inventory driven by libvirt events: VMs, networks and pools (network and pool events are read from `virsh net-event`/`pool-event` over SSH)
- [x] Task log: every action with progress, result and libvirt error text
- [x] Stop a running task with the **x** in the Tasks panel or on the Tasks page: backups, verifies, restores, replication copies, migrations, disk moves, full clones, imports, backup exports and imports, downloads, uploads, evacuations, rolling updates. What the task made so far is removed; the source is never changed. A stopped task answers 409 with a message that says "cancelled" and is not logged as an internal error
- [~] Raw XML editor with schema validation (VMs, networks, creating pools; volumes open)

## VM basics
- [x] Lifecycle: start, shutdown, reboot, force off, reset, pause, resume, delete
- [x] Autostart on host boot; graceful shutdown with timeout before force off
- [x] VNC console in the browser (noVNC, tunnelled over the node's SSH connection)
- [x] Serial/text console in the browser (xterm.js), with Paste text
- [x] Edit CPU, memory, disks, network interfaces through forms (Hardware tab; says whether a change applied live or needs a restart)
- [~] Hot-plug disks and NICs, vCPU and memory changes, online disk resize (done; hot-unplug depends on the guest). USB devices open
- [~] Clock of a guest with a local-time hardware clock (Windows): synced through the guest agent after a start from virticle and after a restore that starts it, when more than 2 s off. Not re-run on a real Windows guest; VMs the node starts at boot are not synced
- [x] Firmware options at creation: UEFI, Secure Boot, virtual TPM, Windows defaults; add/remove TPM on a shut-off VM (BIOS↔UEFI switch on an existing VM not offered)
- [x] Live stats: CPU, memory, disk, network per VM and per host (the newest 10 minutes in memory)
- [x] Performance history: the controller samples every online node and its VMs every 15 seconds and keeps 7 days (`metrics.db`, steps of 15 s, 1 min and 5 min with average and peak); Performance tabs for VM, node and datacenter with zoom, values in a box next to the pointer, markers for start, stop and move, CSV download; a Prometheus `/metrics` endpoint (read-only token). NVIDIA GPU values only for cards the node itself drives
- [x] Guest info on the VM card: IPs, OS, hostname, filesystems (via the agent, with DHCP leases as fallback for IPs)
- [~] Guest agent prompt: adds the channel, lists install commands, detects Windows and attaches or downloads the virtio-win ISO (Windows path untested on a real guest)

## Storage
- [~] Pools: create, start, stop, remove (dir tested; LVM, iSCSI forms and raw XML written but untested; NFS has its own entry below)
- [x] Shared storage over NFS: one dialog mounts a share on every node at the same path, VMs on it migrate without copying disks (live and offline, mixed disks), an unavailable share is shown within seconds and alerted, nothing waits for a dead server, prepare sets up the client, `virt_use_nfs` and the start order. Browser spec 50; lab runs with a real NFS server, including server outages and node restarts, on Debian 13 (one libvirt daemon) and Rocky 10 (SELinux enforcing). Kerberos is not supported; an NFS version 3 only server was tried once
- [x] Volumes: create, resize, clone, upload, download from URL (the node fetches it with curl), delete
- [x] CD-ROM media: insert, swap, eject (live); boot order (config, next start)
- [~] Import existing disks: qcow2, raw, VMDK and single-VM OVA tested; VDI, VHDX untested

## Provisioning
- [~] Create VM from ISO, disk image or empty disk (form works; ISO path not yet tested with a real installer). New VM and Import offer the node's host bridges that no network uses
- [x] Create VM from a cloud image with cloud-init (hostname, user, password, SSH keys, packages, raw user-data); seed ISO built in pure Go
- [x] Templates: mark a VM as a template; full and linked clones (same node)
- [x] Inject SSH keys and user/password when cloning a template (cloud-init seed)
- [~] Same for a Linux template without cloud-init ("Guest setup" in the clone dialog says which way is used, or why not): detected before Clone, written into the clone's disk (virt-customize), packages at first boot; refused with the reason where neither works. Not tried: SUSE, Ubuntu, a guest installed from a real ISO; a failed package install shows only in the guest
- [x] Sysprep on clone, Linux: `virt-sysprep` where the node has it, else "seal template" through the guest agent
- [~] Sysprep on clone, Windows: seal via `sysprep /generalize` through the agent and unattend ISO are written but UNTESTED (no Windows guest in the lab)

## Snapshots and backup
- [~] Snapshots with memory, tree view, revert, delete (done). One kind per VM, chosen by the server; greyed option with a hint in the dialog (see note). Open: quiesce untested with a real agent
- [x] Scheduled snapshots with retention (schedule stored in domain metadata, stored in UTC and shown in the browser time zone)
- [~] Backup and restore to another pool or node (live, no snapshot left behind); export/import a VM as one tar file. Scheduled backups with retention (one unexplained stall seen in testing, not reproduced). The state of an emulated TPM 2.0 is part of every backup and comes back with a restore (new or original VM, another node, export and import), checked on the Rocky pair with a mark that a Linux guest wrote into its TPM. A Windows 11 guest with BitLocker and TPM 2.0 came up without its recovery key after backup, verify and restore (plain, encrypted incremental, encrypted offline; same node and another node). The guest freeze waits up to 2 minutes and a freeze that lands later is thawed at once (before, a guest could stay frozen); not re-run on a real Windows guest. Manifests record the real version. Open: snapshots are not part of a backup
- [x] Incremental backups: after the first full backup only the changed blocks are copied (libvirt checkpoints), a full one after every 7 and whenever a chain cannot be trusted, every file checked by SHA-256, restore and export put the chain together, retention keeps what kept backups need. Browser spec 49; the lab found and fixed killed-QEMU and libvirt-restart cases (Debian 13 with libvirt 11.3, Rocky 10 with libvirt 11.10). Windows: one Windows 11 guest restored from an encrypted incremental backup (see above)
- [x] Verify a backup: every file of it and of the backups it builds on, by size and SHA-256, as a stoppable task on the node that holds it; encrypted backups are checked as stored, without the passphrase. No "last verified" state is kept. Browser spec 57; lab (Rocky pair): plain and encrypted, a flipped byte and a missing earlier backup named, stop half way
- [x] Encrypted backups with age: one switch (Settings > Backup encryption) and one key for VM backups and the controller's scheduled copies. The controller keeps the public key and the private one only inside a passphrase-protected recovery key file; the passphrase is never stored, so scheduled backups are encrypted with nobody present and nothing on the controller decrypts them. Files are encrypted on the node that makes them, before a copy to another node; restore, and an export that puts a chain together, ask for the passphrase and decrypt on the node that holds the backup. The manifest stays readable and records both checksums of every file; chains may mix plain and encrypted backups; the stock `age` tool opens a file by hand. Green or grey dot "Encrypted" on the backup pages and per backup. Node protocol 21. Browser spec 55; lab (Rocky pair, Debian guest with UEFI and TPM): full and incremental, live and shut off, same node and other node, a mixed chain, export and import, stop during encryption and decryption, a scheduled controller copy restored with the recovery key. Overhead on a 1.9 GB backup: about 5 s more and the space of one more copy of the largest disk for a moment. Not done: deleting or replacing the key; encrypting while the backup job writes (qcow2 needs a seekable file)
- [x] Tiered retention (keep last, hourly, daily, weekly, monthly; the rules of restic and Proxmox Backup) for scheduled VM backups, the controller's copies on its server (now daily at a time or every N hours) and `virticle backup -o DIR --keep-*`. One function (`internal/retention`) prunes and computes the schedule assistant's projection: next runs, what is kept once it has run for a while and how far back, and an estimate of the space next to the free space. Pruning never touches the backup just written, one a restore or export reads, or what they build on. Browser spec 54; lab: a 3-minute schedule on the Rocky pair pruned with chains as the preview said. The run that becomes a weekly or monthly point (the last of its week or month) is a full backup, so long-term points keep no chain; the scheduler and the preview decide it with one function (`retention.PointFull`). Open: a missed last run of a week leaves that week's point incremental, with its chain

## Networking
- [x] Virtual network editor: NAT, isolated, routed, open, bridged, DHCP reservations
- [x] Virtual switches: Linux bridges with VLANs, created from the UI. The node saves its settings first and restores them by itself when the controller does not confirm within the confirm time (90 s by default); also after a reboot. Verified on Debian 13 (netplan with systemd-networkd) and Rocky 10 (NetworkManager). Nodes with ifupdown are refused; plain systemd-networkd without netplan runs the same code but was not tried. Open vSwitch is not used
- [x] Bridges set up by hand are virtual switches when their saved definition (NetworkManager keyfiles, netplan YAML) has only settings virticle has: same actions, no label; the first change takes the definition over (NetworkManager: virticle's profiles replace the originals; netplan: the entry is removed from the YAML, comments and other entries stay, the address settings move to the card) under the rollback timer, and a rollback restores the original files byte for byte. Others are usable and say why they are not editable (strict allow-list per system). Node protocol 22. Lab: Rocky 10 and Debian 13, on a second card and with the management address on the hand-made bridge: takeover with MTU and VLAN changes, set-uplinks (a new bond) and add-vlan-address as the first change, bond takeover on both systems, timer rollback and a reboot before the confirmation with identical files afterwards, delete with the address back on the card, refusal cases. Not done: taking over plain systemd-networkd files of the administrator, VLAN interfaces on a hand-made bridge
- [x] Several uplinks per switch as a bond (failover, LACP), the node's own address on a VLAN, MTU per switch. Not tested: LACP against a real switch (the lab network cannot speak LACP), the node's management address moved onto a VLAN (not offered), an MTU larger than 1500 inside guests
- [x] One **Network** tab per datacenter and per node (switches with the node's address in the header, uplinks, networks and the VMs on each with their IPv4 address from DHCP leases or the node's neighbour table; no separate Networks page); a warning when networks differ between nodes, with "Create on node" for bridge networks
- [x] Alerts for a network card without link and for a degraded bond or an LACP bond the switch does not answer
- [x] Speed limit per VM network card (libvirt `<bandwidth>`, applied to a running VM)

## Multi-node
- [x] Node command and two ways to reach a node: Service account (sudo rule for the node command only) or Root. `virticle node prepare` installs it, upgrades it when the controller reports an older version, and `unprepare` removes it. The Nodes page marks a node with an old node command **Outdated** and prepares all such nodes with one login (**Prepare all**, browser spec 56). Verified on the lab in both access modes on Debian 13 nodes and on Rocky 10 nodes (SELinux enforcing): the full browser suite passes on each pair. Found on Rocky: the serial console sometimes closed right after connecting during boot, a bug in the libvirt client library with libvirt 11.10, fixed in `third_party/go-libvirt` (3 of 10 runs failed before, 0 of 12 after); QEMU there takes the CPU flag pdcm away from a running VM, so the cluster baseline now leaves it out. Not run: arm64 nodes; a real OVA import and real virt-sysprep through the node command were covered only by the browser suite where it covers them
- [x] One minimum for every node: libvirt 11.0 and QEMU 10.0 from the distribution (`internal/minver`), no setting to change it. Checked by prepare (installed versions, or what the repositories would install, before anything is installed), by the connection test and on every connection of the controller; a node below it is refused when added and shown Offline with the reason when already added (looked at again every five minutes). Replaces the separate checks for VLANs, incremental backups and the start guard. Verified: Debian 13 nodes (libvirt 11.3, QEMU 10.0) pass; an Ubuntu 24.04 machine (libvirt 10.0, QEMU 8.2) is refused by prepare and shows Offline when added as already prepared. Not run: a Rocky 10 node with this change; the dnf lookup of the repository versions (written, tried on no RHEL-family host)
- [x] Direct node-to-node file transfer with a restricted node key (migration with local disks, restores on another node) and live migration coordinated by the controller, for any pair of Service account and Root nodes. Verified on the lab (Debian 13 nodes): a mixed pair (one Service account node, one Root node) passed offline migration both ways with identical checksums, live migration with disk copy, and backup with checksum and restore. During a live migration of a VM with a 1 GB disk and 512 MB memory the controller machine's total network traffic was under 0.5 MB
- [x] Live migration with pre-flight checks (CPU, networks, free space, passthrough, name clashes, node-to-node path); UEFI and TPM guests verified before the node command replaced the shell script; the node command path was re-run on the lab (see above), UEFI and TPM guests were not re-tested
- [x] Migration with disk copy; snapshot-preserving migration (external snapshots); source disk cleanup; cancel
- [~] Offline migration (done, carries internal snapshots too); UEFI/NVRAM and TPM paths untested
- [x] Encrypted migration streams (QEMU TLS for memory and disk mirror; controller-managed CA, per-node setup/removal, pre-flight warning when unencrypted). Certificate install verified on Debian and Rocky; an encrypted migration between two RHEL-family nodes is untested
- [x] Migration network chosen per node (address for memory, disk and file copies), checked against the other nodes on saving; automatic limit (80% of the link) when migration shares the management card. On the Network tab, **Edit** on a switch or card sets the management and the migration address with checkboxes (management tested before it is saved)
- [x] Evacuate a node (planner, per-VM tasks, cancel, maintenance flag); the node's Updates tab lists the pending package updates in a table
- [x] Rolling node updates: plan first (updates, restarts, where each VM moves, which cannot), then one node at a time: maintenance, live-migrate away, update with dnf or apt, restart if needed, check, move back. The run is stored and resumes after a controller restart; a failure stops it and offers Continue, Put things back or Dismiss. Browser spec 51 on a Debian 13 pair. Not tested: a Rocky 10 run of the whole sequence with real pending updates
- [x] Replication to a standby node: a copy of a VM (qcow2 disks) at most one interval old, planned failover without loss, failover from the newest copy when the node is gone, the old VM marked and powered off when its node returns, replicate back. A counter, never the clocks, decides which copy is old. Browser spec 52; 40 rounds of kill -9 during the merge step left an identical image every time. Work directories of runs and backups cut short by a dead node or controller are recorded and removed when the node is back (at once, or after 10 quiet minutes when the controller was restarted). An emulated TPM 2.0 is copied with the VM. Safeguards against the VM running twice: checks before a failover, a start guard on the nodes, a check when a node reconnects. A Windows 11 guest with BitLocker came up without its recovery key after a planned and an unplanned failover (Rocky nodes, one run)

## Hardware
- [x] Passthrough picker: PCI and USB with IOMMU groups, in-use and safety checks, migration warning. Verified on real hardware with a whole GPU (Quadro P1000 and its audio function: attach, guest sees it, host driver takes it back). USB passthrough, hot-plug of a PCI device and SR-IOV/vGPU are untested or not built
- [x] CPU pinning, CPU model and topology, hugepages, NUMA policy (multi-cell NUMA untested)

## Operations
- [x] HTTPS for the web UI: the **Web access** page (self-signed by default, own certificate, ACME, reverse proxy; interface and port; automatic undo when a change is not confirmed). A fresh install serves HTTPS on port 8463 (8443 is used by many other products); an existing install keeps its listener. ACME issuance is not tested against the real service
- [x] Packages and firewall: `.deb` and `.rpm` with a systemd service, `sudo virticle setup` lets you choose the web address and port (default 8463), checks that the port is free, asks before it opens it in firewalld or ufw, prepare opens the migration ports; a signed apt and dnf repository (fingerprint to be published)
- [x] Node setup in two clicks: guest disk tools always installed, account `virticle-node`, take-over of a node from another controller, "Set up this machine as a node" when the controller's machine can run VMs; `sudo virticle node prepare` without a host prepares the machine it runs on, errors are one line, and the administrator password is asked for on a terminal
- [x] Backup and restore of the controller itself (`virticle backup`, `virticle restore`, Settings > Controller backup, optional scheduled copy on the server with retention rules in a folder of your choice, encryption with a passphrase; the scheduled copy is encrypted to the backup key while backup encryption is on, `virticle restore --key`)
- [x] Bulk actions and tags (tags stored in libvirt `<metadata>`; bulk actions fan out from the browser)
- [x] Released under the Apache License 2.0 (`LICENSE`, `NOTICE` with third-party licences); release packages are not signed yet
- [x] Users and roles (admin, operator, viewer), API tokens, audit log, login rate limiting
- [x] Two-step sign-in (authenticator app, recovery codes, required for nobody, admins or everyone), sign-in through OpenID Connect (tested against dex only) and LDAP or Active Directory (tested against a test directory only), shell recovery (`virticle user reset-2fa`, `reset-password`)
- [x] Alerts: pool full, node down, host memory, failed scheduled job, VM crashed or paused on I/O error, certificate expiry, network link and bond, networks that differ
- [x] Notifications to Slack, Mattermost, Discord or a webhook (signed bodies, retries, summary when many alerts arrive at once). The services themselves were not tried: their messages are covered by tests against a fake receiver
- [x] Resource tree (Datacenter → nodes → VMs, a Templates group closed by default, storage, networks, host NICs) with quick actions (Migrate as a submenu; Clone and Migrate open their dialogs); node page with tabs (summary, performance, VMs, storage, network, updates, settings)
- [x] Theme from the user's DESIGN.md: dark by default, light option, bundled fonts, no animation, explanations only behind a small "?" mark (`Help`). Idle cost measured: 0 running animations, at most one stats request per shown host every 5 s, none on pages without charts
- [x] Browser suite (`make e2e`, 57 spec files with 345 `test(` declarations counted from the source; last full runs: see the release notes) including a no-ellipsis check and a fit check: no element cut off or spilling out of its card at 1280 and 1440 px, both themes, plus phone width
- [x] Move a disk to another pool: live (block copy and pivot) or offline, with pre-flight checks, progress and cancel. Verified on Debian and Rocky; not tested under guest writes; dir pools only tested
- [x] Image catalogue: ten built-in cloud images, downloaded by the node and verified by checksum (same-origin checksum: catches corruption, not a compromised mirror); `<data>/images.json` adds or overrides entries
- [x] Sub pages inside the long tabs (VM Hardware, Snapshots and Backups; node Settings): a list at the left, one sub page at a time, the choice kept in the address (`sec=`)
- [x] Cloud-init profiles (stored in the controller database, passwords as hashes only), selectable when creating and cloning
- [x] Console preview card on the VM overview (still image, refreshed only while visible); the live console opens inside the card, which grows to the page width and the window height, or on a page of its own; a Serial / Graphical switch in the toolbar; no Console tab. Console toolbar: Ctrl+Alt+Del, key combos, paste text, screenshot, full screen
- [x] go-libvirt could wedge a node connection when a stream ended abruptly: fixed with a small patch kept in `third_party/go-libvirt`, plus a per-node watchdog that reconnects an unresponsive connection. Screenshots still use virsh (a separate library bug)

## Planned

What remains, in this order of thought, not of date:

- [ ] **Automatic failover with fencing**: start a VM on its standby without a person, once the old node is provably stopped (power control, a witness, a per-VM choice). What it needs: [docs/backlog-ha.md](docs/backlog-ha.md).
- [ ] **Start the VMs of a dead node elsewhere on shared storage**: the VM definitions are kept by the controller so a VM whose disks are on NFS can start on another node. Specification: [docs/backlog-storage.md](docs/backlog-storage.md).
- [ ] **iSCSI and LVM shared pools**: the same one-dialog setup as NFS. Also in [docs/backlog-storage.md](docs/backlog-storage.md).
- [ ] **Networking ideas** that were considered and are not planned yet: [docs/backlog-networking.md](docs/backlog-networking.md).

### To fix after 0.5.0

Small faults left in 0.5.0:

- [x] **Edit on a libvirt NAT bridge** offers Management on the NAT address. The connection test refuses it; the box should not be offered there.
- [x] **`virticle node prepare`** ends with "Next: add the node" also for a node that is already added.
- [x] **Switch header**: with several addresses the Edit, Add network and Actions buttons wrap to a second line at 1280 px.
- [x] **Clone dialog**: Guest setup and Packages open with the browser's arrow, Advanced with a chevron.
- [x] **Migrate dialog**: the Migrate button is active while the pre-flight checks still say "Checking".
- [x] **Controller backup**: the error for a bad folder starts with a lower-case letter.
- [x] **Late thaw**: when even the thaw of a late freeze fails, it is only written to the controller's log; raise a task or an alert.
- [ ] **Clock sync after a start** does not cover VMs the node starts at boot, a clone that starts, or failover. A better fix: the guest's real time zone in the definition, or a UTC hardware clock for Windows.
- [ ] **One live backup did not freeze** in the Windows test although the agent was connected; the wait is longer now, the cause on Windows is inferred, not shown.
- [x] **Encrypted backup space check** asks for a whole extra copy of the largest disk; a 21 GB disk needed 41.5 GiB free. The check is accurate now (an incremental asks for the changed blocks only); a full backup still needs the extra copy. Encrypting in place would remove it: a node command change, not done.
- [x] **Deleted backups** stay in libvirt's volume list until the pool is refreshed.
- [x] **Screenshot failure hint** names a stuck NFS share when the node is only out of memory.
- [x] **A unit test in `internal/api`** failed once under load and passed in every later run; not identified.

Not tested in 0.5.0:

- [x] **Prepare all** through the web UI on real nodes with this build (the spec mocks the nodes).
- [x] **Edit dialog** on a switch that carries the management address, in the released build.
- [ ] **Backup freeze and clock fixes** on a real Windows guest.
- [ ] **Guest setup on clone** on SUSE, Ubuntu, a guest installed from an ISO, and a node prepared with `--minimal`.
- [x] **The dnf repository** after the release (the apt list was checked).
- [x] **The service's wider write paths** (`/mnt`, `/media`, `/srv`) in a packaged install.
- [x] **Import dialog** with a host bridge, an Open vSwitch bridge in New VM, and the light theme of the new console card and help marks.

Found while working through the list above (2026-11-15), open:

- [ ] **Add node through a jump host** fails on a new controller with "unable to authenticate": the controller's own key is not authorised on the jump host and the dialog does not say so.
- [ ] **A node taken over from another controller** needs a manual Fix for migration encryption; the take-over could do it.
- [ ] **Saving a controller backup folder** does not write a backup; one appears only after the schedule is switched off and on.
- [ ] **dnf asks to import the signing key twice**; the install guide says once.
- [ ] **Migration of a VM on an Open vSwitch host bridge** is not tested (New VM, Add NIC, Edit NIC and Import are fixed and tested).
- [ ] **Operation traces** show a node command verb, not the programs it ran inside; needs a node command change (protocol bump). Not verified on the lab: clone with a password, quiesced snapshot, migration, scheduled tasks.
- [ ] **Frozen-guest alert**: the record is in memory only (a controller restart forgets it); it appears at the next alert pass, not at once.
- [ ] **Backups deleted on an NFS pool**: the refresh on the other nodes is not tested on the lab.
- [ ] **Task results**: no downtime figure for a migration and no current step for a running task (the code records neither).
- [ ] **Alerts for a user's failed task** are never sent to notification destinations; a scheduler alert replaced by a user failure of the same job never sends its "clear".
- [ ] **Command line setup** not tested on a machine that never had Virticle, with the RPM, with a firewall active, or `node add` through a jump host.
- [ ] **Unit tests that time out under load**: `internal/dirauth` TestTransport (5 s) and `TestQRCodeDecodes` each failed once while several agents were building; both pass alone.
- [x] **Bootloader and kernel modules**: `node prepare` loads a missing KVM module and keeps it in `/etc/modules-load.d/virticle.conf` (a blacklisted or failing module stops the step with the reason). It never edits the boot loader or reboots: where a kernel argument is needed (IOMMU for passthrough, or one that blocks KVM) it names the exact arguments for the node and where they go.
- [ ] **Migration cleanup, not verified or still open**: the file identity on NFS (and removal there, which uses a plain rename) was not tried in the lab; a disk move still removes its new volume after a failure without an identity check; restore and clone were not reviewed for the same weakness; a VM whose hand-over answer is lost stays paused on the source until the target answers (no limit).
- [ ] **Hardening of the public repository**: secret scanning and push protection are off, `main` is unprotected, Actions are pinned by tag (the owner has not decided; nor on rewriting the commit author and the old attribution lines).

Built for 0.5.0 (see the entries above): bridges set up by hand as virtual switches, the Edit dialog for management and migration addresses, the console inside the Overview card, Guest setup on a clone of a template without cloud-init, the folder of the controller's scheduled copies, the Templates group in the tree, the "?" help marks, `node prepare` on the machine itself, the longer backup freeze, and the clock sync after a start.

Built for 0.4.0: encrypted backups, tiered retention with the schedule assistant, full weekly and monthly points, backup verify, the TPM state in backups, stopping running tasks, sub pages, one minimum for node versions, Prepare all, the work directory sweep, and the setup that chooses the web address and port.

## Design notes

**Sysprep.** Two mechanisms, chosen per node and guest:
- Linux, node has libguestfs: run `virt-sysprep` and `virt-customize` on the
  clone's disk while it is off. Works on any Linux image, no guest cooperation.
- Linux, no libguestfs: "seal" the template once through the guest agent
  (clear machine-id, remove host keys, `cloud-init clean`), then give each
  clone a cloud-init seed ISO built by the controller. Needs cloud-init in the
  template.
- Windows: the SID can only be changed by Windows itself. "Seal template"
  runs `sysprep /generalize /oobe /shutdown` through the guest agent; each
  clone gets an `unattend.xml` (account, password, hostname) on an attached
  ISO so first boot is unattended.

**Snapshot type.** The web UI has no type choice; the server decides in one place (`chooseSnapshotKind`, internal/api/snapkind.go) and the Take snapshot
dialog only shows which state of "Save the running state too" is possible (greyed with a hint when only one is). A VM keeps one kind until all its snapshots are deleted. Without
snapshots: a running VM gets external snapshots (guest keeps running, survives live migration; verified on Rocky 10 with SELinux, libvirt 11.10:
create, revert and delete of a chain), a shut-off VM an internal one. Nodes whose QEMU is confined by AppArmor (Debian 13) only get internal
snapshots: libvirt's per-VM profile denies writes to a backing file whenever it generates the profile from the domain definition (`deny "<file>" w`
for read-only backing files), no include or local override can beat a deny rule, and so block-commit fails with "Permission denied" for a shut-off
VM, after a revert or a restart. Internal snapshots of a running VM always save its state; with a raw UEFI variable store none can be taken
while it runs.

**App metadata.** One root element `<virticle:meta>` in namespace
`https://virticle.invalid/ns/1` holds one child per feature
(`template`, `snapshot-schedule`, `tags`). Clones do not inherit it.
Before the rename the namespace was `https://virt-proj.invalid/ns/1` (prefix
`vp`): it is still read when no new element exists, and every write stores the
complete content under the new namespace and removes the old element.

**Access levels.** Every API route needs an entry in `routeLevels`
(`internal/api/access.go`); an unlisted route requires admin and fails a unit
test. Behind a reverse proxy, name it with `-trusted-proxy` so the same-origin
check and per-IP rate limit use the forwarded host and client address.

**One controller per set of nodes.** Scheduled snapshots are driven by the
controller from VM metadata, so two controllers pointed at the same nodes
would both run every schedule. `-no-scheduler` turns one of them off, and an
alert appears when another controller's scheduled snapshots are seen. Controllers of different versions must never manage the same nodes (an older one deletes backups that a newer incremental chain needs).

The project is called Virticle (earlier working name: `virt-proj`).

## Build and run

    make build                      # needs Go and Node.js
    ./bin/virticle -listen 127.0.0.1:8080 -data ./data

The first visit creates the administrator account. `data/` holds the SQLite
database and the controller's SSH key. Add each node from the Nodes page
(**Add node**) or prepare it with `virticle node prepare`; it installs the node
command and the controller's key. See `docs/install-node.md`.
