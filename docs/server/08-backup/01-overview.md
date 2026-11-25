## 💾 Backups and Snapshots

ODAC keeps two kinds of copies of your data. Both are encrypted, deduplicated and stored with the same engine ([Kopia](https://kopia.io)), and neither stops an app while it is copied.

| | Backups | Snapshots |
|---|---|---|
| **Where** | A target you add: another disk, an SMB or NFS share, S3-compatible storage or WebDAV | ODAC's own data directory (`<data dir>/backup/snapshots`) |
| **When** | On the schedule of a job you create | Before a deploy, before an app is deleted, before an ODAC update, periodically, and when you ask |
| **Kept** | Thinned with the job's density, for as long as you like | Short term: a week of periodic snapshots and the last five of each event |
| **Survives the server** | Yes, on a remote target | No: snapshots are lost with the server |
| **Managed by** | You (`odac backup ...`) | ODAC (`odac snapshot ...` only lists, takes, deletes and restores) |

A snapshot is a quick way back from a risky operation, such as a deploy whose migration went wrong. A backup is what you restore from when the server itself is gone. A fresh server comes with snapshots turned on and no backup job, because a real backup needs a target only you can provide; until you add one, `odac backup coverage` keeps saying so.

### How the Pieces Fit
- **Targets** are where backups go. Each target holds one encrypted repository per server, under the server's permanent id, so several ODAC servers can share one target. See [Targets and Keys](02-targets-and-keys.md).
- **Keys** encrypt a target's repository. ODAC generates a strong key (or takes yours) and shows it once when you add the target. Without the key and without the server, the backups cannot be opened.
- **Jobs** say what to back up (some apps, every app, ODAC's own state), to which target, how often and how densely to keep old backups. See [Jobs and Retention](03-jobs-and-retention.md).
- **Consistency** is chosen per piece of data: a recognized database is read through its own dump tool, everything else as files. Every backup records how consistent it is. See [Consistency](04-consistency.md).
- **Snapshots** need no setup. See [Snapshots](05-snapshots.md).
- **Restores** bring apps back in place, as new apps, or from another server's directory on a shared target. See [Restoring](06-restore.md).
- **Verification** reads a sample of every repository each week and warns about damage. See [Verification and Damage](07-verification.md).

### Getting Started
```bash
# 1. Add a target. The key is shown once: save it, then type "yes".
odac backup target add --name offsite --s3 my-bucket --endpoint s3.eu-central-1.amazonaws.com \
  --access-key AKIA... --recovery-file ./offsite-recovery.json

# 2. Back up every app, and ODAC's own state, every 6 hours.
odac backup job create --name nightly --target offsite --all --server --every 6h

# 3. Check what is protected and how the jobs stand.
odac backup coverage
odac backup status
```

A new job runs right away; later runs follow its interval.

### Warnings
`odac backup coverage` (and the dashboard) lists what deserves attention:

- **No job**: the only copies are snapshots, which are lost with the server.
- **Unprotected apps**: apps that hold data (a volume, or a database the recipe declares) but are in no job. A job with `--all` covers apps added later too.
- **Missing links**: a job backs up an app but not the database app it links to with `env.linked`, so a restore would bring the app back without the data it changed together with.
- **Only local targets**: every target is on this server, which does not survive the loss of the server, the account or ransomware.
- **Same disk**: a target on the disk that holds the data itself only protects against deleting by mistake.
- **Conflicts**: writes to a target stopped because another machine writes under this server's id there (usually a cloned VM). See [Targets and Keys](02-targets-and-keys.md#clones-and-adopting-a-directory).
- **Damage**: a verification found damaged data. See [Verification and Damage](07-verification.md).
- **Orphaned servers** and **shared targets without a budget**: see [Shared targets](03-jobs-and-retention.md#shared-targets).

The [Security Model](08-security.md) explains how backups are contained, and [Limits and Caveats](09-limits.md) lists what to know before relying on them.
