// Not a Go module in any real sense — a sentinel so `go build ./...` and
// `go test ./...` skip this directory.
//
// npm packages sometimes ship Go source: `flatted` carries a whole Go package,
// and without this it turns up in `go list ./...` as
// hecate/ui/node_modules/flatted/golang/pkg/flatted. Anything under
// node_modules that fails to compile would then break a Go build that has
// nothing to do with it. Go excludes nested modules from a parent's `./...`,
// and npm never touches this file.
module hecate-ui

go 1.26.5
