## ⏪ Restoring

Data is never restored automatically. Every restore shows what it will do first (the apps, how each one comes back, and for an in-place restore how much recent data is lost) and starts only after you confirm. A restore waits for the server's backup slot, so it never meets a backup or maintenance in the same repository, and only one restore runs at a time.

### Choosing a Restore Point
```bash
odac backup points -t offsite              # runs on a target, newest first
odac backup points -t offsite --app blog   # runs that hold blog
odac snapshot list                         # snapshots on this server
```

Each point lists the apps it holds with their [consistency label](04-consistency.md) and whether a database was dumped. Restoring an app always brings back the apps it links with `env.linked` from the **same run**, so an app and its database never come back from different moments.

### As a New App (the default for backups)
```bash
odac backup restore apps -t offsite --run <run> --apps blog
odac backup restore apps -t offsite --run <run> --apps blog --name blog=blog-old,blog-db=blog-old-db
```

The live app is not touched. The copy gets a new name (`<name>-restored` unless you choose one with `--name`), and its links and any environment value naming another app of the group as a host are renamed with it, so the copy never writes to the live database. Because they would clash with the live app, a new app does **not** get:

- domains and published host ports (traffic stays with the live app);
- host networking (the copy runs on the bridge), devices, and its API grant (grant them again with `odac app api` if needed).

Everything dropped is listed in the result. Git and script apps can only be restored in place, since their source is in the repository and needs a build.

### In Place
```bash
odac backup restore apps -t offsite --run <run> --apps blog --in-place
odac snapshot restore --run <run>          # snapshots restore in place by default
```

An in-place restore puts the app's data back the way it was at that point. What to expect:

- **It is downtime.** The restored apps are stopped for the restore, and so are apps that link to them without being restored, which would otherwise write to a database being replaced. They start again at the end.
- **Data written since the point is lost**: the preview shows how long that is, per app.
- **A safety snapshot comes first**: the current state is snapshotted (event `pre-restore`), so the restore itself can be undone. If the snapshot cannot be taken, nothing is touched and the restore is refused. With snapshots turned off it is refused unless you pass `--skip-safety`.
- **It needs free disk space** as large as the restored data plus 1 GiB, because nothing is written over live data: each volume gets a staging directory, the restored files are swapped in by rename, and the old files are kept aside until the whole restore, database loads included, has succeeded. Any failure before that puts every volume and every app back as it was.
- **Databases are loaded with their own tool.** The data volume is emptied, the image initializes an empty database from the app's environment, and the dump is loaded into it (`psql`, `mysql`/`mariadb`, `mongorestore --drop`, `clickhouse-client` plus re-attached parts). Redis is not loaded but placed: the RDB becomes the data file (and the AOF file when `appendonly yes` is on the command line).
- An in-place restore is refused while the app is being deployed.

A server that stops in the middle of a restore (an update, a crash) rolls it back on its next start from the restore journal.

Follow a restore with `odac backup restore list`; `odac backup restore cancel --id <id>` stops and rolls back the one in progress.

### From Another Server
Every server's directory on a shared target can be restored from, which is also how you move apps to a new server:

```bash
# On the new server: add the same target with its key.
odac backup target add --name offsite --s3 my-bucket --endpoint ... --key <key of the target>
odac backup target servers -t offsite
odac backup points -t offsite --server <old server id>
odac backup restore apps -t offsite --server <old server id> --run <run> --apps blog
```

Another server's directory is opened read-only: listing and restoring work, nothing there is ever written or deleted. `--key` opens a directory whose key differs from the target's. The safety snapshot of an in-place restore is still taken on this server.

### ODAC's Own State on a Fresh Server
When a server is lost, a job with `--server` lets a freshly installed ODAC take its place: config, certificates (with the DKIM and mTLS CA keys), databases and mail.

1. Install ODAC on the new machine. Do not create apps on it: the state restore refuses a server that already has apps.
2. Add the old target with the old key (from the recovery file):
   ```bash
   odac backup target add --name offsite --s3 my-bucket --endpoint ... --key <key>
   ```
3. Stage the old server's state. ODAC picks the other server's directory (or asks when there are several) and the newest run holding ODAC's state, shows it and asks:
   ```bash
   odac backup restore state -t offsite
   ```
4. Confirm the restart. On its next start ODAC swaps the staged state in before it reads its config; the replaced directories are kept under `<data dir>/backup/server-restore/<id>/previous`. This server's own backup targets, their keys and its server settings stay as they are. Every restored app comes back inactive.
5. Bring the apps' data back in place, with the command the previous step prints:
   ```bash
   odac backup restore apps -t offsite --server <old server id> --run <run> --in-place
   ```

`odac backup restore state --cancel` drops a staged state before the restart. The state restore is available on the local CLI only, never from the dashboard.

### Without ODAC
The repositories are plain Kopia repositories. With the key, `kopia repository connect` and `kopia snapshot restore` reach the files even without ODAC; dumps are stored as single files you load with the database's own tool.
