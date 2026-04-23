module github.com/token-bay/token-bay/plugin

go 1.23

toolchain go1.26.2

require (
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	github.com/tailscale/hujson v0.0.0-20260302212456-ecc657c15afd
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Note: no `require github.com/token-bay/token-bay/shared` directive is present
// because no plugin source imports shared/ yet. `go mod tidy` removes unused
// requires but keeps this `replace` so the require resolves locally when a
// feature plan adds the first shared-side import. Do not delete this replace
// when it appears dangling — it is intentional.
replace github.com/token-bay/token-bay/shared => ../shared
