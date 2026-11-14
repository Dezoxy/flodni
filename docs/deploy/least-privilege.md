# Less container privilege

Status: **designed, not verified on a host.** Nothing on this page was run in
a container. The profile has not been loaded, the narrower `docker run` flags
have not been started, and the AppArmor syntax has not been parsed
(`apparmor_parser` was not available where it was written). Treat it as a
checklist to verify, not a recipe that works. The default deployment is
unchanged.

## The default, and why

The image runs with:

```
--device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor:unconfined
```

This is what CI and `make e2e` use.

- `/dev/fuse`: the kernel side of the FUSE channel. Docker does not create it
  in a container; `--device` also adds it to the device cgroup allow list.
- `SYS_ADMIN`: the `mount(2)` that attaches the FUSE filesystem needs
  `CAP_SYS_ADMIN` in the mount namespace's owning user namespace. A normal
  container's user namespace is the host's, so the capability must be real.
- `apparmor:unconfined`: Docker's `docker-default` profile contains
  `deny mount,`, so the mount fails even with `SYS_ADMIN`.

Docker's default seccomp profile needs no change: it allows `mount` and
`umount2` only when the container holds `CAP_SYS_ADMIN`, which the default
already adds.

## What Flodni actually does

Read from the code, not assumed:

- `internal/daemon/main.go` calls go-fuse's `fs.Mount` with
  `AllowOther: true` and no `DirectMount`. go-fuse (v2.12.1) then runs the
  `fusermount3` helper, which opens `/dev/fuse`, calls `mount(2)` and hands the
  file descriptor back. Flodni does not call `mount(2)` itself.
- The image has no `USER`, so the daemon and `fusermount3` run as root.
  `fusermount3` is setuid root, which makes no difference for a process that is
  already root. Because it runs as root, `allow_other` does not need
  `user_allow_other` in `/etc/fuse.conf`; libfuse applies that check to
  non-root callers.
- `docker/docker-entrypoint.sh` only calls `fusermount3 -uz` on the mount path
  (stale-layer cleanup at start, between runs, at shutdown). It never mounts.
- The code makes no `chown`, `setuid`, `mknod`, raw-socket or `chroot` calls
  outside the vendored engine.
- The mount path (`/mnt/flodni-virtual` by default) is normally a bind mount
  from the host so that a media server in another container sees the FUSE
  layer. That needs mount propagation (below).

So the privileges the FUSE mount needs are: `/dev/fuse`, `CAP_SYS_ADMIN`,
and an AppArmor profile that does not deny the mount. Seccomp is already
fine. `user_allow_other` only matters if the image ever stops running as root.

## What cannot be removed

`CAP_SYS_ADMIN` stays. A non-root user does not avoid it either: the setuid
`fusermount3` still needs the capability in the container's user namespace.
The only ways to drop it are to mount outside the container (the host opens
`/dev/fuse` and passes the descriptor; go-fuse supports a `/dev/fd/N`
mountpoint for that) or a user-namespaced runtime. Both are larger changes
and not covered here.

What the narrower option removes is everything around it: the other default
capabilities, and the unconfined AppArmor domain.

## The narrower option

Four parts. All are opt-in.

1. `--cap-drop ALL --cap-add SYS_ADMIN`. Docker's default set (CHOWN,
   DAC_OVERRIDE, FOWNER, SETUID, SETGID, KILL, NET_RAW, ...) is not used by
   the code above and is dropped. Two things to check on your host:
   - if the library or state directories are owned by another uid and not
     readable or writable by root, add `DAC_OVERRIDE` (or `DAC_READ_SEARCH`
     for read-only files) and `CHOWN`/`FOWNER` as the failure shows;
   - a peer port below 1024 would need `NET_BIND_SERVICE`.
2. `--device /dev/fuse`, as before.
3. `--security-opt apparmor=flodni-fuse` instead of `apparmor:unconfined`.
   The profile is [docker/apparmor/flodni-fuse](../../docker/apparmor/flodni-fuse):
   `docker-default` with `deny mount,` replaced by two rules that allow only
   `fstype=fuse` and `fstype=fuse.*` with `rw`, `nosuid` and `nodev` onto
   `/mnt/flodni-virtual/` (never `fuseblk` or `fusectl`, which are denied), and
   the umount narrowed to the same path. Edit the paths if `FLODNI_MOUNT_PATH`
   differs.
4. Mount propagation, only if another container reads the mount: bind the
   mount path with `rshared` and make the host directory a shared mount.
   This is independent of the privilege change and works the same in both
   variants.

```bash
# On the host, once per boot, if the bind source is not already shared:
sudo mount --bind /srv/flodni/virtual /srv/flodni/virtual
sudo mount --make-rshared /srv/flodni/virtual

docker run -d --name flodni \
  --device /dev/fuse \
  --cap-drop ALL --cap-add SYS_ADMIN \
  --security-opt apparmor=flodni-fuse \
  -v /srv/flodni/config.json:/config.json:ro \
  -v /srv/flodni/state:/state \
  -v /srv/flodni/library:/mnt/flodni-real \
  --mount type=bind,source=/srv/flodni/virtual,target=/mnt/flodni-virtual,bind-propagation=rshared \
  flodni:dev
```

The paths are placeholders. The repository ships no compose file, so there is
no commented variant to add; the equivalent compose keys are `devices`,
`cap_drop`, `cap_add` and `security_opt: ["apparmor=flodni-fuse"]`.

### Load the profile

Linux with AppArmor enabled (Ubuntu, Debian with `apparmor` installed):

```bash
sudo apparmor_parser -QT docker/apparmor/flodni-fuse   # syntax check only
sudo apparmor_parser -r -W docker/apparmor/flodni-fuse # load or reload
sudo aa-status | grep flodni-fuse
```

A loaded profile is lost at reboot unless it is also copied to
`/etc/apparmor.d/` (then `systemctl reload apparmor` loads it at boot).

### Verify it works

Verification is the open item. In order:

1. The container starts and stays up; `docker logs` shows
   `FUSE mounted at ...`.
2. `docker exec flodni mountpoint -q /mnt/flodni-virtual` exits 0, and
   `docker exec flodni flodni healthcheck` passes.
3. From the host (or the media server container), `ls` the mount path and read
   a file. If propagation is wrong the directory is empty here and full inside.
4. `docker exec flodni grep CapEff /proc/1/status` shows only the
   `SYS_ADMIN` bit (`0000000000200000`), and
   `docker inspect -f '{{.HostConfig.SecurityOpt}}' flodni` shows the profile.
5. Negative check: another mount in the container is refused, e.g.
   `docker exec flodni mount -t tmpfs tmpfs /tmp` fails with permission
   denied, and `dmesg | grep apparmor` shows `DENIED` with `operation="mount"`.
6. `docker stop flodni` finishes inside the 10 s grace period and the mount
   is gone from the host.
7. `make e2e` with the flags changed in `tests/e2e/run.sh` (it uses the
   default flags today and is not changed here).

A mount denied by AppArmor shows in `dmesg` or the journal as
`apparmor="DENIED" operation="mount"` with the profile name; a missing
capability shows as `fusermount3: mount failed: Operation not permitted`
without an AppArmor line.

### Roll back

Remove `--cap-drop ALL` and swap `--security-opt apparmor=flodni-fuse` for
`--security-opt apparmor:unconfined` (and add back `--cap-add SYS_ADMIN` if
it was the only capability kept), then recreate the container. The profile
can stay loaded; unload it with `sudo apparmor_parser -R
docker/apparmor/flodni-fuse`.

## Risk, honestly

- The narrower option helps against a container escape through the dropped
  capabilities and through paths AppArmor still denies (`/proc`, `/sys`, other
  mounts). It does not remove `SYS_ADMIN`, which is itself a broad capability.
  Do not describe it as unprivileged.
- AppArmor mount rule matching is picky (fstype, options, trailing slash).
  The rule here is written from the documentation and has not been tried.
  If the mount is denied, the audit line names the exact fstype and path to
  put in the rule.
- Hosts without AppArmor (Fedora, RHEL, Docker Desktop) have nothing to load;
  there, `--security-opt apparmor=...` is an error or ignored. SELinux hosts
  need their own policy and are not covered.

## Sources

Cited from the projects' documentation; the links were not re-fetched when
this page was written.

- Docker, runtime privilege and Linux capabilities (`--cap-add`, `--cap-drop`,
  `--device`, `--security-opt`):
  <https://docs.docker.com/engine/containers/run/#runtime-privilege-and-linux-capabilities>
- Docker, default seccomp profile (`mount`/`umount2` conditional on
  `CAP_SYS_ADMIN`): <https://docs.docker.com/engine/security/seccomp/>
- Docker, AppArmor security profiles (`docker-default`, `--security-opt
  apparmor=`): <https://docs.docker.com/engine/security/apparmor/>
- Docker, bind propagation (`rshared`):
  <https://docs.docker.com/engine/storage/bind-mounts/#configure-bind-propagation>
- The `docker-default` template this profile derives from:
  <https://github.com/moby/profiles/blob/main/apparmor/template.go>
- libfuse, `fusermount3(1)` and `mount.fuse3(8)` (setuid helper,
  `user_allow_other` in `/etc/fuse.conf`):
  <https://github.com/libfuse/libfuse/tree/master/doc>
- Linux kernel, FUSE: <https://docs.kernel.org/filesystems/fuse.html>
- `capabilities(7)`: `CAP_SYS_ADMIN` covers `mount(2)` and `umount(2)`.
- AppArmor policy reference, mount and umount rules:
  <https://gitlab.com/apparmor/apparmor/-/wikis/AppArmor_Core_Policy_Reference>
- go-fuse v2.12.1, `fuse/mount_linux.go` and `MountOptions.DirectMount`
  (fusermount by default, `mount(2)` directly only when asked).
