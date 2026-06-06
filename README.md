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
