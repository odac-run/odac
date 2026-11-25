## ⚠️ Limits and Caveats

Things to know before relying on backups.

### Protection
- **Snapshots are lost with the server.** Only a job on a remote target survives the server, the account or ransomware. A disk on the same server, or on the same disk as the data, protects against deleting by mistake only.
- **The key is the only way in.** A lost key with a lost server means the backups cannot be opened.
- **One target is a single point of failure.** Verification finds damage but cannot repair it; keep two independent targets for what matters.

### Consistency
- A standalone MongoDB is dumped collection by collection and labelled `live`; only a replica set member gives one point in time.
- MySQL tables in MyISAM, Aria or MEMORY make the dump `live` (taking them consistently would block the app's writes).
- MariaDB's dump ignores the lock wait setting: a dump waiting on a metadata lock ends only after 5 minutes without output or at the run's time limit.
- Redis is skipped (and its last save read as a file) when there is not enough memory for the fork. Redis's RDB is compressed, so it deduplicates poorly.
- A PostgreSQL password kept only in a file (`POSTGRES_PASSWORD_FILE`) cannot be used for a dump; that database is read as files.
- Apps with an unknown storage engine and no hook are read live. Pausing an app is never done for you.
- ClickHouse tables on a non-local disk (for example S3 disks) are not frozen; `user_files` and `format_schemas` are not backed up.
- File-system snapshots (btrfs, ZFS, LVM) are not used yet.

### Restores
- An in-place restore is downtime for the restored apps and the apps linked to them, and needs free space as large as the restored data plus 1 GiB.
- A database whose data volume is a mount point of its own (another disk mounted at the app's volume) keeps the data it replaces inside the volume until the restore commits. An image that initializes only an empty directory, such as PostgreSQL with `PGDATA` at the mount point, then refuses to start, and the restore is rolled back. A volume that is an ordinary directory (the default) is not affected.
- Redis restored in place gets its AOF only when `appendonly yes` is on the command line; an `appendonly` set in a config file is not seen (a warning says so).
- Git and script apps can only be restored in place.
- A new app does not get the original's domains, published ports, host networking, devices or API grant.
- Snapshots restore data, not code.
- ODAC's own state is restored only onto a freshly installed server with no apps, from the CLI.

### Space
- Deleted backups give their space back only after several maintenance runs (a few days with daily maintenance).
- The free space floor (the larger of 10% and 5 GB) is kept even under a budget.
- S3 and WebDAV have no notion of free space; without a budget, only density and maximum age limit them. Fair sharing between servers applies only to disk, SMB and NFS targets.
- Each server keeps a cache per repository on ODAC's data disk (`<data dir>/backup/cache`).

### Operations
- One heavy backup runs at a time per server, but a snapshot has its own slot and may run beside a backup, so a deploy's snapshot never waits for a remote upload. Both run at low CPU and I/O priority within their limits.
- An I/O weight is dropped automatically on disks whose scheduler does not support it (most cloud disks).
- SMB and NFS targets need ODAC to run in its container (the daemon mounts them); a development server running outside Docker supports disk, S3 and WebDAV only. SMB passwords and other mount values containing a comma are refused.
- A target's location and key cannot change; use a new target and job.
- Repairing damaged pieces is a manual step with the `kopia` tools; new backups do not rewrite them.
- Kopia's error correction is off.
- A server's directory on a shared target is never removed by ODAC, even when its server is gone: you decide.
