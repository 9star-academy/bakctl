# Backup reliability implementation plan

**Goal:** Fix the reviewed data-loss and false-success bugs, add regression tests, and document the supported backup/restore contract.

**Architecture:** Keep the existing CLI, engine, storage and scheduler packages and use only the Go standard library. Use versioned backup metadata, safe staged restoration, exact job matching, locked configuration transactions and explicit error propagation. Preserve restoration of existing v1 artifacts where their type can be identified safely.

**Tech stack:** Go 1.24+, gzip/tar, AES-GCM, SQLite/pg_dump CLI, S3 SigV4, systemd.

## Scope and decisions

- Repair implemented local/S3, SQLite/PostgreSQL and file/directory functionality. Remove unsupported MySQL, WebDAV, zstd and notification claims; reject ineffective webhook settings explicitly.
- Restore to a new path, validate data before publishing the destination, reject unsafe archive paths and never overwrite existing user data silently.
- SQLite requires a successful online backup; never copy the live main database as fallback. PostgreSQL output must not include stderr.
- Retention and restore select only the exact job's artifacts, support legacy names and generate collision-resistant new names. Surface retention failures.
- Config mutations reload under an OS process lock and atomically write only the intended mutation; completed backups must not revive or overwrite changed jobs.
- Propagate config and scheduler failures to a nonzero CLI exit code. Use injectable scheduler dependencies for tests without touching /etc.
- Correct S3 canonical encoding, pagination and local storage atomic/path-safe I/O.
- Keep bounded memory for new backups where feasible using private temporary files and streaming. Any format change must retain a tested legacy reader.

## Tasks

1. Backup/restore: reproduce file mismatch, WAL loss, job-prefix deletion, symlink failure and timestamp collision in tests; implement and add corruption/traversal/legacy regression coverage. Files: internal/backup/*.go, internal/crypto/*.go.
2. Storage: add independent SigV4 vectors, pagination and local write/path tests; fix implementation. Files: internal/storage/*.go.
3. Scheduling: test failed/missing systemctl, invalid schedules, install/remove behavior and quoting; propagate errors and keep testable filesystem boundaries. Files: internal/scheduler/*.go.
4. Configuration/CLI: tests first for failed persistence, concurrent snapshots and task status merging, reliable CLI restore/retention selection and schedule error exit codes. Files: internal/config/*.go, internal/cli/*.go.
5. Delivery: accurate README with consistent privileges, install/restore commands, prerequisite tools, format compatibility, limitations and restore drill; add push/PR CI and release checksums. Files: README.md, .github/workflows/*.yml, scripts/install.sh.
6. Validation: go test -race -coverprofile, go vet, bash syntax, CLI local encrypted file/directory and SQLite WAL recovery, Linux/macOS cross-builds; review concurrent changes and git diff --check. Actual cloud/systemd checks remain explicitly separate if services are unavailable.

## Acceptance

- Restored file bytes, directory contents/symlinks and SQLite committed rows match the source.
- Wrong keys, truncated/corrupt data, missing hot-backup tools and unsafe tar entries fail without a published partial destination.
- No cross-job pruning, no same-second overwrite and no stale config overwrite.
- Failed persistence, retention and systemd commands cannot produce a successful exit.
- Existing v1 backup fixtures are covered by compatibility tests.
- Meaningful success and failure-path coverage; target aggregate statement coverage >=80%.

No dependency addition, automatic release, push or production-state mutation is part of this change.
