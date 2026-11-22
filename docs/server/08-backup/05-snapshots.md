## 📸 Snapshots

Snapshots are short-term restore points that ODAC takes on its own, right before risky moments. They are stored with the backup engine in ODAC's own data directory, with a key generated on the server and never shown, and they are lost with the server. They are on by default and need no job.

### When a Snapshot Is Taken
| Event | What is taken |
|---|---|
| `deploy` | Before an app with data is deployed: the app and the apps it links with `env.linked`, as one group, because a migration usually changes the linked database rather than the app. A stateless app's deploy takes its linked database. |
| `delete` | Before an app is deleted, while it still runs. |
| `update` | Before an ODAC update: ODAC's own state only. |
| `pre-restore` | Before an in-place restore, so the restore itself can be undone. |
| `periodic` | For every app with data, about ten times the last snapshot's duration apart, between one hour and one day. |
| `manual` | `odac snapshot take --app <app>` |

**Deploy snapshots never stop a deploy.** The snapshot starts with the build and runs beside it, so it usually adds no time. If it is still running when the build is done, the deploy waits up to three minutes, then gives the snapshot up (its dump is stopped before the new container starts, so it never runs beside a migration) and goes on. The deploy's build log gets a `[Snapshot]` line, and `odac snapshot status` marks the deploy as one without a snapshot. A deploy snapshot is never cancelled by its own deploy.

### Restoring a Snapshot
```bash
odac snapshot list --app blog
odac snapshot restore --app blog          # lists blog's snapshots and asks which run, in place
odac snapshot restore --run <run> --new-app --name blog=blog-copy
```

A snapshot restores **in place** by default, with its whole group. Data written after the snapshot is lost; the restore shows how much time that is and asks before it starts. See [Restoring](06-restore.md) for what an in-place restore involves. Snapshots bring back data, not code: ODAC does not roll an app's code back.

A deleted app's snapshots are kept for **seven days** after the delete. During that time restoring one of them (`odac snapshot restore --app <app>`) brings it back as a new app, with the name it had if that is free.

### How Long Snapshots Are Kept
- Periodic snapshots are thinned with the normal density and kept for at most seven days.
- Event snapshots are kept separately: the last five of each event per app.
- A deleted app's snapshots go seven days after its delete.

### Disk Share
Snapshots use the disk dynamically, the way ODAC's swap does: the repository may hold at most `maxDiskPct` (default 25%) of the free space plus its own size. Since its own size counts as space it could give back, the budget shrinks as apps fill the disk and grows again as they free it. Swap and snapshots sharing one disk with the same rule take about 40% of it together at most.

On top of that, the same free space floor as for targets applies: the larger of 10% of the disk and 5 GB. Below the floor the budget is zero.

- Before a snapshot, its size is estimated from what the previous one of the same series added. If it would not fit, older snapshots are thinned first (periodic ones first, then older event snapshots, each app's newest event snapshot last); if it still does not fit, it is not taken.
- While a snapshot is written, free space is checked every two seconds and the snapshot is given up when the floor comes close. A snapshot is all or nothing: one that fails part way is deleted at once.
- Below the floor, every snapshot may go, including each app's last one, and `odac snapshot status` warns: running apps and databases come before snapshots.
- A snapshot that could not be taken for lack of space is a warning; a deploy goes on regardless.

### Settings
```bash
odac snapshot settings                    # show
odac snapshot settings --max-disk 15      # 1 to 50 percent
odac snapshot settings --off              # take no new snapshots
```

Turning snapshots off takes no new ones but keeps the existing ones (thinning and maintenance go on); delete them with `odac snapshot delete --run <run>`. With snapshots off, an in-place restore has no safety copy and is refused unless you pass `--skip-safety`.

### Status and History
```bash
odac snapshot status      # disk share, schedule, last deploys, warnings
odac snapshot history     # snapshots taken and given up, maintenance, verifications
```

A periodic snapshot gives way to a deploy and is retried after it; a busy server postpones periodic snapshots for up to two hours. A snapshot does not wait for a running backup: it has its own slot, so a deploy's snapshot never queues behind a long upload to a remote target.
