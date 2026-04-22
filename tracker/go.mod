module github.com/token-bay/token-bay/tracker

go 1.23

toolchain go1.26.2

// Note: no `require github.com/token-bay/token-bay/shared` directive is present
// because no tracker source imports shared/ yet. `go mod tidy` removes unused
// requires but keeps this `replace` so the require resolves locally when a
// feature plan adds the first shared-side import. Do not delete this replace
// when it appears dangling — it is intentional.
replace github.com/token-bay/token-bay/shared => ../shared
