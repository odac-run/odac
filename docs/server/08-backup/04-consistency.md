## 🧩 Consistency

Backups never stop an app: the zero-downtime promise holds while they run. How each piece of data is read is chosen per piece, not per app, so a database, an SQLite file and plain files inside one app can each be read differently.

### Consistency Labels
Every stored piece carries a label, shown in the restore list and the history:

| Label | Meaning |
|---|---|
| `consistent` | One point in time, as the database itself would replay it: a dump taken inside a single transaction, frozen ClickHouse parts, SQLite's online backup. |
| `crash-consistent` | The state after a power cut, which a database recovers from on start: files of an app that was stopped when they were read. |
| `live` | Read while the app kept writing. Files that changed during the read are read again (up to three passes); a file that keeps changing is taken from the previous backup's sound copy (`stale`) or stored as read (`torn`). A dump that could not be taken as one moment is `live` too. |

A backup marked `live` restores fine for most apps, but a database restored from one may need its own recovery. Give an app a better label by declaring its engine (below) or a hook.

### Databases
A recognized database is dumped by its own tool, in a separate dump container made from the database app's own image, joined to the app's network namespace, so the dump's memory never counts against the database and the out-of-memory killer can only ever stop the dump. The volumes holding its data directory are then left out of the file backup, because a live data directory read as files is torn.

| Engine | Method | Notes |
|---|---|---|
| PostgreSQL | `pg_dumpall` as the superuser, `pg_dump` as an app user | `--lock-wait-timeout`, so a dump never waits on a lock forever. |
| MySQL | `mysqldump --single-transaction` | Lock waits bounded through `lock_wait_timeout`. Tables in MyISAM, Aria or MEMORY are warned about and make the dump `live`. |
| MariaDB | `mariadb-dump --single-transaction` | Its dump ignores the lock wait setting, so a wait on a metadata lock ends only with the stalled-dump limit (5 minutes without a byte) or the run's time limit. |
| Redis, Valkey, KeyDB | The replication RDB stream (`redis-cli --rdb`) | Skipped with a warning when the host, or the container's limit, lacks the memory a fork may need; the last `dump.rdb` is read as a file instead. |
| MongoDB | `mongodump --archive` | One point in time only on a replica set member (`--oplog`); a standalone server, the official image's default, is dumped collection by collection and labelled `live`. |
| ClickHouse | MergeTree tables frozen with `ALTER TABLE ... FREEZE`, the rest as SQL | No locks. Definitions of Kafka, URL, S3 or Distributed tables are kept, their rows are not read. |
| SQLite | SQLite's online backup API, by ODAC | Found by file header in any app; the container needs no `sqlite3`. WAL databases never block writers. |

An engine is recognized from the recipe's declaration first, then from the image name, and is always confirmed by running the tool in the image. A dump that cannot be used (tool missing, no credentials found, not enough memory for Redis) is a warning in the run's record, and that data falls back to files; only a Docker daemon that does not answer defers the run. Dumps are taken uncompressed so deduplication works on them; the engine compresses them.

**Credentials** are read from the app's environment and reach the dump tool only on its standard input, never on its command line or in its environment, which `docker inspect` and `ps` would show. A PostgreSQL password that exists only in a file (`POSTGRES_PASSWORD_FILE`) cannot be read, and that database is backed up as files with a warning.

### Declaring an Engine or a Hook
An app whose image name is not recognized, or one with its own storage engine, can say how to read it in its recipe's `backup` member:

```json
{ "backup": { "engine": "postgres" } }
```

```json
{
  "backup": {
    "hook": {
      "dump": ["my-tool", "export", "--stdout"],
      "restore": ["my-tool", "import", "--stdin"],
      "file": "export.bin",
      "consistency": "consistent",
      "covers": ["/var/lib/my-tool"]
    }
  }
}
```

A hook runs in the app's image, network namespace and environment, as an argument list, never through a shell. Its output is the backup and `restore` reads it back on stdin. `covers` names the container paths the dump replaces in the file backup. Without `consistency` a hook's dump counts as `live`, since ODAC cannot know what the command reads. An app with neither a recognized engine nor a hook is read as files and labelled `live`.

### Resource Limits
Each helper has its own CPU quota, memory limit (no swap past it), low CPU and I/O weight and a process limit, derived from the host: by default a quarter of memory (512 MiB to 2 GiB) and half the cores for the backup helper, an eighth of memory (256 MiB to 1 GiB) and one core for a dump. Dumps are read at 64 MiB/s at most; a slow reader slows the database's sending and so its load. Pausing an app is never used.
