# Contributing to gnss_ptp_exporter

Thanks for helping! Please read [GOVERNANCE.md](GOVERNANCE.md) first. In
short: **every change starts with an issue and arrives as a pull request.**

## Build and test

Requirements: Go (the version in `go.mod`), Linux for running the exporter.

```sh
go build .
go test ./...
go test -race ./...            # needs a kernel/arch the race detector supports
scripts/fuzz.sh 30s            # every fuzz target for 30 s
golangci-lint run              # golangci-lint v2
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Changes to a parser should survive a longer fuzz run. A crashing input that
the fuzzer writes to `testdata/fuzz/` is committed with the fix as a
regression test.

## Workflow

1. Find or open an issue and say that you are working on it.
2. Fork the repository and create a branch from `main`, named after the
   issue, e.g. `fix/12-sky-dedupe`.
3. Make focused commits. Commit messages: short imperative summary line
   (≤ 72 characters), blank line, explanation of *why*. Reference the issue.
4. Add or update tests. Bug fixes need a regression test that fails
   without the fix. New fixtures come from real devices where possible;
   remove positions and identifiers before committing them.
5. Update the README (metrics table, flags) and `CHANGELOG.md` when
   behavior changes.
6. Open a pull request using the template. Start the description with
   `Closes #<issue>`.

## Code guidelines

- Measure only. A change that makes the exporter send anything other than
  GET management messages to ptp4l, or anything other than poll requests to
  the receiver, needs an issue and an explicit opt-in flag.
- Keep metric names stable; a rename or a change in meaning is a breaking
  change and goes into the changelog.
- Treat all input as hostile: no panics on malformed data, bounded memory,
  timeouts on every I/O.
- Keep dependencies minimal; new ones are discussed in an issue first.
- Code comments explain *why*, not *what*.

## Licensing

By submitting a contribution you agree that it is licensed under the
project's MIT License.
