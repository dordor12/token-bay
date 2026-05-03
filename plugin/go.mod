module github.com/token-bay/token-bay/plugin

go 1.25.0

toolchain go1.26.2

require (
	github.com/creack/pty v1.1.9
	github.com/google/uuid v1.6.0
	github.com/rs/zerolog v1.35.1
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	github.com/tailscale/hujson v0.0.0-20260302212456-ecc657c15afd
	github.com/token-bay/token-bay/shared v0.0.0-00010101000000-000000000000
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/sys v0.43.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Resolves the `shared` require to the in-repo sibling module for go.work-based
// development. The pseudo-version above (v0.0.0-00010101000000-000000000000) is
// what `go mod tidy` synthesizes for a replace-only dependency.
replace github.com/token-bay/token-bay/shared => ../shared
