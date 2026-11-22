## 🎯 Targets and Keys

A target is a place backups are written to. You define it once, and any number of jobs can use it. Credentials live with the target, in ODAC's config, and never appear in a list, a log or the dashboard.

### Kinds of Target

| Kind | Option | Example |
|---|---|---|
| Another disk on this host | `--disk <host path>` | `--disk /mnt/backup` |
| SMB share | `--smb //<host>/<share>` | `--smb //nas.lan/backups --username odac` |
| NFS export | `--nfs <host>:<export>` | `--nfs 10.0.0.5:/srv/backups` |
| S3-compatible (AWS, B2, R2, Wasabi, MinIO) | `--s3 <bucket> --endpoint <host>` | `--s3 odac --endpoint s3.us-west-002.backblazeb2.com` |
| WebDAV | `--webdav <url>` | `--webdav https://dav.example.com/backups` |

Common options: `--path` (a subdirectory inside an SMB or NFS share), `--username`, `--password`, `--domain` and `--version` (SMB), `--version` (NFS), `--region`, `--prefix`, `--access-key`, `--secret-key` and `--no-tls` (S3). A secret you leave out is asked for without echo. SFTP and FTP are not supported.

```bash
odac backup target add --name usb --disk /mnt/usb-backup
odac backup target add --name nas --smb //nas.lan/backups --path odac --username odac
odac backup target add --name offsite --s3 my-bucket --endpoint s3.eu-central-1.amazonaws.com --region eu-central-1 --access-key AKIA...
odac backup target list
```

**How each kind is reached.** ODAC never mounts anything itself. A disk path is bind-mounted into the backup helper container through the Docker API, so it can be any path on the host, even though ODAC runs in a container. SMB and NFS are Docker `local`-driver volumes that the daemon mounts on the host, created for one helper and removed with it. S3 and WebDAV are reached over the network from the helper.

**Paths that are refused.** Mounting a host path amounts to host root, so system trees (`/bin`, `/etc`, `/usr`, `/proc`, `/run`, `/var/lib/docker` and the like), `/tmp`, `/var/tmp`, ODAC's own data directory, and roots such as `/`, `/home`, `/mnt`, `/opt`, `/srv` or `/var` themselves are refused; a directory inside the latter is fine. SMB and NFS values containing a comma (user name, password, share path) are refused, because mount options are one comma-separated string. A WebDAV URL must not carry a user name or password; use `--username` and `--password`. Two targets may not point at the same storage.

**Local and remote.** A disk is always *local*. A network target is local when it resolves to this host, its interfaces or a Docker bridge, and *remote* otherwise. A server with only local targets is warned about. A disk target on the same file system as ODAC's own data is also flagged as **same disk**: it protects against deleting by mistake, not against losing the disk.

A target's location cannot change later (its backups are there): you can rename it or replace its credentials with `odac backup target update`, and new credentials are saved only once the repository opens with them. To move to new storage, add a new target and a new job. Deleting a target keeps its backups on the storage; a target that still has jobs, or deleted jobs whose backups wait to be purged, cannot be deleted.

### Layout on the Target
Each server writes into its own directory, named after its permanent id (`<data dir>/server-id`, created on first start and never changed), not its host name:

```
<target>/
  3f9c.../                 # this server's id
    server.json            # owner file: id, name, sequence number, last write
    usage.json             # filesystem targets: last measured size
    repository/            # the Kopia repository
  a71e.../                 # another server sharing the target
```

The name next to the id comes from `ODAC_SERVER_NAME`, or the host name. The repository format is Kopia's own, so even without ODAC you can open it with the `kopia` command line and your key.

### Keys
Every repository is encrypted; there is no unencrypted option. The key belongs to the target: all jobs writing to one target share its repository, its deduplication and its key.

- By default the server generates a 160-bit key (39 characters, in groups of four).
- `--key` sets your own passphrase instead, at least 16 characters, with no leading or trailing space or control character.
- The key is shown **once** when the target is added, together with what a new server needs to open the backups. `--recovery-file <file>` writes it as a recovery file (mode 0600). The target is saved only after you type `yes` to confirm you have stored the key.
- While the server is running, `odac backup key -t <target>` shows it again (`--out <file>` writes the recovery file). Only the operator sees it; app tokens never do, and the dashboard only relays it to you.
- The key cannot be changed later.

> [!IMPORTANT]
> If the key is lost **and** the server is gone, the backups on that target cannot be opened by anyone. Keep the recovery file somewhere that is not this server.

The recovery file holds the server's id and name, the target's description (without its storage credentials, which you get from your storage provider) and the key.

### Clones and Adopting a Directory
A cloned VM keeps the original's server id and would write into the same directory. To catch that, each write to a target first advances a sequence number in `server.json`, after the server has saved the number locally. When the number on the target is already at or ahead of the one the server is about to write, another machine writes under this id: the server stops writing to that target (reading and restoring still work) and `odac backup coverage` reports a conflict.

If you know why (for example you reinstalled the server and added the target again), take the directory over:

```bash
odac backup target adopt -t nas
```

Adding a target again after a reinstall needs the key it was created with; `--adopt` takes over this server's old directory in the same step. Check and write are not atomic: two clones writing at the same moment can both pass once, and the one behind stops at its next write.

### Seeing Other Servers
```bash
odac backup target servers -t nas
```

lists every server directory on the target, this server first. It is what a restore from another server, a migration, chooses from (see [Restoring](06-restore.md#from-another-server)).
