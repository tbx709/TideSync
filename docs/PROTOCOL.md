# TideSync wire protocol v1

The agent is a read-only HTTP/1.1 service. Everything is JSON except file and
archive bodies. A client never sends anything but GET requests, so the agent has
no write surface at all.

Base path: `/api/v1`. All paths are relative to the agent's `root` and always
use `/` as separator, on every platform.

## Authentication

When `server.token` is set, every endpoint except `/healthz` requires:

```
Authorization: Bearer <token>
```

The comparison is constant time. `server.allow_cidrs` additionally filters by
source address (CIDR or plain IP).

## Errors

Non-2xx responses carry a JSON body:

```json
{"error": "file not found on agent", "code": ""}
```

| Status | Meaning |
| --- | --- |
| 400 | malformed request, unsafe path, or a path that is not a regular file |
| 401 | missing or wrong token |
| 403 | client address outside `allow_cidrs`, or file not readable by the agent |
| 404 | path does not exist on the agent (or the endpoint is disabled) |
| 405 | method other than GET/HEAD |
| 500 | scan or read failure |
| 503 | too many concurrent transfers, retry later |

## `GET /healthz`

Unauthenticated liveness probe.

```
ok uptime=1h2m3s
```

## `GET /api/v1/info`

```json
{
  "version": 1,
  "app": "tidesync-agent",
  "agent_version": "1.0.0",
  "os": "linux",
  "arch": "arm64",
  "hostname": "fileserver",
  "root": "/srv/shared",
  "server_time": "2024-05-01T12:00:00Z",
  "features": ["sha256", "range", "archive", "manifest"],
  "read_only": true
}
```

The client refuses to continue when `version` differs from its own, so a partial
upgrade fails loudly instead of corrupting anything.

## `GET /api/v1/manifest?path=REL[&hash=1|0]`

The complete description of one subtree. `path` is optional (`""` or omitted
means the agent's root).

`hash=1` forces digests, `hash=0` suppresses them; without the parameter the
agent's `hash_mode` decides.

```json
{
  "version": 1,
  "root": "share/data",
  "generated_at": "2024-05-01T12:00:00Z",
  "hashed": true,
  "truncated": false,
  "entries": [
    {"path": "sub", "type": "dir", "mtime": "2024-04-01T08:00:00Z", "mode": 493},
    {"path": "sub/a.txt", "type": "file", "size": 12,
     "mtime": "2024-04-01T08:00:01Z", "mode": 420,
     "sha256": "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"},
    {"path": "link.txt", "type": "symlink", "mtime": "2024-04-01T08:00:02Z", "target": "sub/a.txt"}
  ]
}
```

Notes:

- entries are sorted by path, so a client can stream-compare them;
- empty directories are included so a destination reproduces the tree shape;
- `sha256` is absent when the file is larger than `hash_max_size_mb` in `auto`
  mode, or when `hash_mode` is `never`;
- in-progress staging files (`.tidesync-tmp-*`) are never listed;
- `truncated: true` means the tree exceeded `manifest_max_entries`; clients
  abort rather than synchronise an incomplete tree.

The agent does not reuse a scan that was generated before the request arrived,
so a client that asks right after a change always sees it. A scan produced while
the request was queued is shared, which coalesces simultaneous clients.

## `GET /api/v1/file?path=REL`

Raw file content.

- `Accept-Ranges: bytes` and `Range` support (single range), so an interrupted
  transfer resumes from where it stopped;
- `ETag` is `"sha256:<hex>"` when a digest is known, otherwise
  `"sm:<size>-<mtime-nanos>"`;
- `X-Tidesync-Sha256` carries the digest when the agent has it cached;
- `Last-Modified`, `HEAD`, and conditional requests behave as `http.ServeContent`
  defines them;
- the client verifies the received bytes against the manifest digest regardless
  of transport level checksums.

## `GET /api/v1/archive?path=REL`

A `tar.gz` stream of the subtree, used for cold starts where per-file request
overhead would dominate (many small files on a Raspberry Pi). Directory and
symlink members are included. Clients still verify every file against the
manifest digest and re-download individual files that fail, so the archive is an
optimisation and never a correctness dependency. Disable with
`archive_enabled: false`.

## Client behaviour (informative)

1. `GET /info` — protocol check and reachability probe.
2. `GET /manifest?hash=1` when `verify` is `sha256`.
3. Compare against the local journal and disk, then transfer what changed.
4. Write each file to `<name>.tidesync-tmp-<tag>` in the destination directory,
   `fsync`, verify, apply timestamp/permissions, then rename over the target.
5. Update the journal (`.json`, atomically replaced) and, in mirror mode, delete
   local files that the manifest no longer lists.

Retries: transport errors and 5xx/408/429 are retried with exponential backoff
(`retries`, `retry_backoff`, capped at 30 s). 4xx responses are permanent and
fail that file immediately. A digest mismatch is retried immediately from
scratch.

## Compatibility

Protocol version 1 is the only version so far. Adding optional JSON fields or
new endpoints is backwards compatible; changing the meaning of an existing field
requires a version bump.
