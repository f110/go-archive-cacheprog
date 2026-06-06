# go-archive-cacheprog

**This software is for technical verification.**

A [`GOCACHEPROG`](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching) implementation that stores the Go build cache as a single zip archive file.

Because the entire cache lives in one file, it is easy to ship as a CI cache artifact or a container image layer. The archive is a regular zip, so entries can be read with random access — there is no need to expand everything at startup.

## Install

```sh
go install go.f110.dev/go-archive-cacheprog@latest
```

## Usage

Point `GO_ARCHIVE_CACHE_FILE` at the archive path and set `GOCACHEPROG` to this binary:

```sh
export GO_ARCHIVE_CACHE_FILE=/path/to/cache.zip
export GOCACHEPROG=go-archive-cacheprog
go build ./...
```

If the file does not exist yet, it is created on the first build. If it does exist, its contents are loaded and used as the cache.

### Compression

The compression method for newly written entries can be selected with `GO_ARCHIVE_CACHE_COMPRESSION`:

| Value     | Method     | Notes                                                                  |
| --------- | ---------- | ---------------------------------------------------------------------- |
| `deflate` | Deflate    | Default. Compatible with every zip tool.                               |
| `zstd`    | Zstandard  | zip method 93. Similar ratio to deflate but several times faster. Standard zip tools cannot read these entries. |
| `store`   | No-op      | No compression. Lowest CPU cost, largest file.                         |

Entries kept from an existing archive are copied without recompression, so a single archive may contain a mix of methods. Reading transparently handles all three.

### Scratch directory

While the cacheprog is running, every `put` body and every `get` cache hit is materialized as a file on disk so that the `go` toolchain can read it by path. These files live in a per-process scratch directory that is removed on close.

By default, the scratch directory is created under the OS temp directory (`$TMPDIR` or `/tmp`). Override the parent with `GO_ARCHIVE_CACHE_TMPDIR` to put it on a different filesystem — for example a tmpfs ramdisk or a dedicated cache volume:

```sh
export GO_ARCHIVE_CACHE_TMPDIR=/mnt/fast-scratch
```

If the directory does not exist it is created with `mkdir -p` semantics. The scratch subdirectory itself is always removed on close; only the random subdirectory under it is touched.

### Example: CI

```sh
# Restore the archive produced by a previous job.
restore-cache /path/to/cache.zip

export GO_ARCHIVE_CACHE_FILE=/path/to/cache.zip
export GOCACHEPROG=go-archive-cacheprog
go build ./...
go test ./...

# Save the archive, now including any entries added by this job.
save-cache /path/to/cache.zip
```

## How it works

- **get**: looks up the entry in the archive, extracts it into a temporary directory, and returns the disk path. The temporary directory is removed when the process exits.
- **put**: writes the object received from `go` to the temporary directory and records it in an in-memory pending list. Nothing is written to the archive yet.
- **close**: if there are pending entries, writes a new zip that merges the existing entries with the pending ones, then atomically replaces the archive via `rename`. Duplicate `ActionID`s are overwritten by the most recent put.

### Archive layout

Each zip entry is named `<ActionID hex>-<OutputID hex>` and its body is the cache object itself. Size and modification time come from the zip metadata.

```
sha256(actionA)-sha256(outputA)
sha256(actionB)-sha256(outputB)
...
```

## Limitations

- `put` bodies are buffered in memory, so this tool is not suited for very large objects (typical Go build cache objects are at most a few MB).
- Requests are handled sequentially. `go` may send requests in parallel, but the current implementation serializes them.
- Concurrent writers to the same archive file are not supported — the last process to close wins.
