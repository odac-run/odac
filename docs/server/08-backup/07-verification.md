## 🔍 Verification and Damage

Backups share their pieces: every backup is logically complete, but identical data is stored once and referenced by every backup that contains it. Damage to one shared piece on the storage therefore affects every backup using it. ODAC checks each repository on its own, without being asked.

### What Runs
Once a week per target, and for the snapshot repository, ODAC verifies the repository in the maintenance slot (never beside a backup, a maintenance or a restore of the same repository):

- every object of this server's backups is looked up, and the storage file holding it must exist;
- a random sample of the files is read in full, which decrypts them and checks their hashes: **5%** on disk, SMB and NFS targets and the snapshot repository, **1%** on S3 and WebDAV, where every byte read is billed or crosses a link. Over successive weeks the samples cover the repository.

The first verification runs right after a target's first maintenance, about an hour after its first backup, so a target that does not keep what it is given is noticed early. A verification that cannot run (storage unreachable) is tried again after six hours.

### When Damage Is Found
- The verification is recorded as failed with the damage it found, in `odac backup history` (or `odac snapshot history`).
- The target is listed under **damage** in `odac backup coverage` and in `odac backup status`; the dashboard gets the same warning. For the snapshot repository it is a `corrupt` warning in `odac snapshot status`.
- The next verifications of that repository read **every** file, and the warning clears only once such a full verification finds nothing: a sampled one could miss the damaged file. On S3 or WebDAV that full read is billed like any download.

What to do:

1. Find out why: a failing disk, a share that lost writes, or files changed on the storage by hand.
2. Restore what you need from another target. The real protection against damage is two independent targets; a server with a single target is warned about.
3. Backups taken after the damage do not repair the damaged pieces: deduplication finds them already stored. Repair is a manual step with the `kopia` tools (for example `kopia snapshot fix remove-invalid-files`), or start a fresh target and job and delete the damaged one once the new one holds what you need.

Kopia's error correction is deliberately off: it is experimental in Kopia, fixed into a repository's format when it is created, and it only protects against small damage inside a file.

### Verifications During an Update
A verification keeps running in its helper container while ODAC updates; the new server takes it over and records its result as adopted.
