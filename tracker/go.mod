module github.com/token-bay/token-bay/tracker

go 1.23

toolchain go1.26.2

// Note: no `require github.com/token-bay/token-bay/shared` directive is present
// because no tracker source imports shared/ yet. `go mod tidy` removes unused
// requires but keeps this `replace` so the require resolves locally when a
// feature plan adds the first shared-side import. Do not delete this replace
// when it appears dangling — it is intentional.
replace github.com/token-bay/token-bay/shared => ../shared

require (
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
