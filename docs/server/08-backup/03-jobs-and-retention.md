## 🗓️ Jobs and Retention

A job is what to back up, where, how often and how densely to keep the past. You can have as many jobs as you like: the same apps can go to two targets with different schedules, or different apps to different targets.

### Creating a Job
```bash
odac backup job create --name nightly --target offsite --all --server
odac backup job create --name db-hourly --target usb --apps postgres,api --every 1h --density high
odac backup job create --name media --target nas --apps photos --every 1d --max-age 365 --budget 500g
```

| Option | Meaning | Default |
|---|---|---|
| `--target` | The target the job writes to. It cannot change later. | (required) |
| `--apps a,b` / `--all` | The apps to back up. `--all` also covers apps added later; a fixed list would silently leave a new app out. | (one is required) |
| `--server` | Also back up ODAC's own state: config, certificates (including the DKIM and mTLS CA keys), databases, mail. | off |
| `--every` | How often: `15m` to `31d`. | `1d` |
| `--density` | How densely old backups are kept: `low`, `normal`, `high` or a number. | `normal` |
| `--max-age` | Days after which backups go, whatever the density. | none |
| `--budget` | The most this job's backups may reference on the target, like `50g`. | none |

`odac backup job list` shows the jobs, `odac backup job update -j <job> ...` changes one (options left out keep their value, `--no-server` drops ODAC's state), and `odac backup job run -j <job>` runs one now without moving its schedule.

### How Runs Are Scheduled
- One heavy backup runs on the server at a time; the others wait their turn. A backup and maintenance never meet in one repository.
- A busy server (load above 1.5 per core, or less than a tenth of memory free) postpones a scheduled run, for at most two hours. A run you start by hand is not postponed.
- A run may take four times the job's interval, within one to twelve hours. A large first backup cut off by that limit keeps what it uploaded, and the next run continues from there.
- A failed run is retried after 5 minutes, 15 minutes, 1 hour and 4 hours, but never later than the job's next regular slot.
- Missed slots (the server was down) are not made up one by one: the next run is the next slot on the job's own clock.

**Deploys come first.** A dump holds a transaction open, and a migration waiting behind it would queue every query behind it. When an app, or an app linked to it, starts deploying, its dump is cancelled; the rest of the run goes on, and that app is backed up under the same run once the deploy has been idle for two minutes. `odac backup status` shows such apps as waiting.

**A database is not dumped twice.** When another job, or a snapshot, dumped the same app within the last 15 minutes (or a quarter of the job's interval, if shorter), the stored dump is read back instead, and the backup carries the same consistency label. A dump taken before the app, or an app linked to it, was last deployed or restored in place, or before ODAC last started, is never read back, since a migration or a restore may have replaced its data. ClickHouse is always read afresh.

### Thinning
Old backups are not deleted oldest first; they are thinned, so the recent past stays dense and the far past stays reachable, only sparser. The rule is continuous: the gap between a kept backup and its neighbour grows with its age (gap about age / k, where k is the density).

| Density | k | Backups kept over ten years by an hourly job |
|---|---|---|
| `low` | 2 | about 39 |
| `normal` | 3.5 | about 62 |
| `high` | 7 | about 107 |

What this means in practice:

- **Stable.** Buckets are anchored to the calendar, not to each backup's age, so a backup kept today is not deleted tomorrow just because it got a day older. Thinning every hour and thinning once a year later delete exactly the same backups.
- **The newest backup of a job is never deleted**, so a job that has been failing longer than its maximum age still has something to restore.
- **One run is one unit.** An app and its linked database are kept or deleted together, so they never come back from different moments.
- **Jobs never touch each other's backups**, even on a shared repository.
- A run that stored only some of its pieces (partial) stays out of thinning and goes once a newer complete run of its job exists.

### Space: Budget and Free Space Floor
Deleting a backup does not return its space at once. Its unique data becomes garbage that maintenance gives back only after at least three full maintenance runs more than five hours apart, which with daily maintenance takes a few days. Space decisions are therefore taken after maintenance and count garbage on its way back, so the same shortage is never deleted for twice.

- **Budget.** A job over its `--budget` loses backups, densest region first (relative to age), its oldest last and its newest never.
- **Free space floor.** A filesystem target (disk, SMB, NFS) always keeps the larger of 10% of its disk and 5 GB free, even without a budget and even under one, because maintenance itself needs room and a full disk cannot even delete. Without a budget the target is otherwise used to the end. S3 and WebDAV have no notion of free space: only the density rule and budgets apply there.

Maintenance runs once a day per target, and again after six hours while garbage is still coming back or after deletions. A target whose free space falls under the floor after a backup gets its maintenance at once.

### Shared Targets
Several servers may write to one filesystem target with no protocol between them: the target itself is the shared table. After each maintenance every server writes its measured size and the time into `usage.json` in its own directory and reads the others'.

- A server counts as **active** while its usage file is younger than seven days.
- The **fair share** is the space the active servers hold together with the free space, less the floor, divided by the number of active servers.
- When the floor is reached, only servers holding more live data than their share thin, each in proportion to how far above it is. A server at or below its share deletes nothing, so a server that writes little never loses backups because another writes a lot. Space a server leaves unused stays available to the others until the floor is reached.
- A server whose usage file went stale is an **orphan**: its space counts as taken, and **no server ever deletes another server's data**. `odac backup coverage` lists orphaned directories; restore from them, or remove the directory on the storage yourself once you no longer need it.
- `odac backup status` shows this server's fair share and the other servers it found; `odac backup coverage` suggests a `--budget` for this server's jobs on a shared target that has none, which is how you divide the space deliberately.

This applies to disk, SMB and NFS targets only; S3 and WebDAV report no free space.

### Deleting a Job
```bash
odac backup job delete -j media            # keeps its backups
odac backup job delete -j media --purge    # deletes them too
odac backup job retired                    # deleted jobs whose backups are kept
odac backup job purge -j media             # delete those backups later
```

By default a deleted job's backups are kept: they are no longer thinned and stay restorable under the job's name. Purged backups are removed with the target's next maintenance.

### History and Status
```bash
odac backup status                 # next run of each job, each target's size, maintenance, verification and share
odac backup history -j nightly     # finished runs, maintenance and verifications, newest first
```
