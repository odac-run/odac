## 🔒 Security Model

A backup reads every app's data and can write to any disk on the host, so its design keeps that reach inside a container that can do as little as possible.

### The Backup Helper
Each backup, restore, maintenance or verification runs `odac-backup` in a helper container started for that operation and removed after it. The orchestrator itself does not contain the backup engine: Kopia and its storage clients are linked into `odac-backup` alone, so their failures and memory stay out of the long-running ODAC processes. A helper that crashes or runs out of memory loses only that one operation.

The helper:

- runs ODAC's **own image**, by image id rather than tag, so an update in progress never mixes two releases;
- **never gets `docker.sock`**, which would be root on the host. When a database is dumped, the ODAC server starts the dump container and streams its output into the helper itself;
- drops every Linux capability except the file ones the operation needs (reading other users' files for a backup; setting owners and modes for a restore), with `no-new-privileges` and a read-only root file system;
- has a required memory limit (no swap past it), a CPU quota, low CPU and I/O weight, and a process limit;
- sees app directories **read-only**, and writes only to its cache, the target and a restore's staging directory;
- has **no network at all** for a disk, SMB or NFS target; for S3 and WebDAV it joins a network of its own (`odac-network-backup`) where containers cannot reach each other.

A dump container runs the database app's image inside that app's network namespace, with its entrypoint replaced by the dump command, and always with logging off, because its output is the database's contents.

### Secrets
- A run's secrets (the repository key, storage credentials) travel to the helper on its standard input, never in its environment or command line, which `docker inspect` and `ps` show. Messages from the helper are masked against them.
- Database credentials reach the dump tool the same way: a fixed wrapper turns standard input into a private file in the dump container's memory, and the tool runs with no secret in its arguments or environment.
- SMB and NFS options, which carry the share password, become a Docker volume created for one helper and removed with it.
- Target credentials and keys are stored in ODAC's config. No list, log, history or dashboard row carries them. A target's key leaves the server only when it is first shown, and through `odac backup key`, which only the operator may call.
- The snapshot repository's key is generated on the server and never shown. It stays out of ODAC's own backups.

### Who May Do What
- Every `backup.*` and `snapshot.*` API action is closed to app tokens, whatever an app's grant says: a target is host root (it mounts a host path), a restore overwrites another app's data, and a key opens every backup. See [API Access](../03-app/08-api-access.md).
- Showing a key and restoring ODAC's own state require the operator, and the state restore exists on the local CLI only, never on the dashboard.
- Disk targets cannot name system trees or ODAC's own data directory (see [Targets](02-targets-and-keys.md)).

### Restores and Other Servers
- Another server's directory on a shared target is opened read-only. A server never writes to, thins or deletes another server's data, not even an orphan's.
- Restored file names are checked before anything is written: an entry that would climb out of the restore directory, pass through a restored symlink, or be restored twice fails the restore, because a migration reads a repository another machine wrote. Paths under the apps directory must not contain symlinks.
- Restored data is staged next to the live data and swapped in by rename, so a failed restore leaves the old data in place.

### Clones
A cloned VM writes under the same server id. The sequence number in each server directory's owner file stops the machine that falls behind before it writes anything (see [Clones](02-targets-and-keys.md#clones-and-adopting-a-directory)).
