module github.com/jroedel/dropin-forms

// One directive, and no separate `toolchain` line. With both, CI installs the
// first and Go downloads the second into the module cache, after which every
// cache restore unpacks a toolchain already on disk and emits one tar warning
// per file. In the repository this process came from that was 11,541 of 11,958
// lines in a deploy log, which is how a log stops being read.
// The patch version is pinned deliberately, and tidy will keep it.
//
// CI installs exactly what this line says -- actions/setup-go reads
// go-version-file: go.mod -- so `go 1.26` or `go 1.26.0` means CI builds
// against the first 1.26 release and govulncheck reports every standard
// library advisory fixed since. That failed the vulnerability scan with 15
// findings against a tree that was clean on a developer machine running a
// patched toolchain, which is the worst shape for a check to fail in: red in
// one place and green in another, for a reason neither reports.
//
// Still one directive and no separate toolchain line, which is the rule in
// .claude/skills/writing-go. Raise this when a Go patch release fixes an
// advisory the scan reports.
go 1.26.8

require (
	github.com/BurntSushi/toml v1.4.0
	modernc.org/sqlite v1.34.5
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/stripe/stripe-go/v83 v83.2.1 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260908163034-4bcc4b2ee518 // indirect
	golang.org/x/tools v0.50.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
)

tool golang.org/x/vuln/cmd/govulncheck
