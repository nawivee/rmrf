# rmrf

Recursively deletes a directory with progress tracking via HTTP.

## Usage

```bash
go run main.go /path/to/dir
```

Check progress from another terminal or remote machine:

```bash
curl http://localhost:8698
```

Output:

```
status:  in_progress
started: 2026-06-24T12:00:00Z
files:   142830
freed:   48.3 GB
elapsed: 2m15s
```

## Install

```bash
go install github.com/nawivee/rmrf@latest
```

Or run directly from GitHub without installing:

```bash
go run github.com/nawivee/rmrf@latest /path/to/dir
```

## Build

```bash
GOOS=linux GOARCH=amd64 go build -o rmrf main.go
```
