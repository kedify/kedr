# Repository instructions

- For Go source or CI changes, run the relevant tests and `make lint` before handoff. Use the Go version required by `go.mod` and the golangci-lint version pinned in `Makefile` and `.github/workflows/ci.yml`.
- Keep the Makefile and CI golangci-lint version pins in sync. If the lint tool cannot analyze the configured Go version, resolve that toolchain issue before treating lint as passed.
