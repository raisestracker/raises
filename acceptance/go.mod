// Keeps `go test ./...` and `go vet ./...` in the server module out of
// acceptance/node_modules. The suite's Go harness lives in cmd/raises-acceptance.
module raises.local/acceptance

go 1.18
