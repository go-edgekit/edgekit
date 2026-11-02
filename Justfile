set ignore-comments := true

default:
  @just --list

# Run all checks that CI runs.
check: lint actionlint zizmor test race vet tidy-check

fmt:
  golangci-lint fmt

lint:
  golangci-lint run

test pattern="":
  go test -count=1 -shuffle=on {{ if pattern == "" { "" } else { "-run '" + pattern + "'" } }} ./...

race:
  go test -race ./...

vet:
  go vet ./...

tidy-check:
  go mod tidy -diff

security:
  govulncheck ./...

actionlint:
  actionlint

zizmor:
  zizmor --collect=all .
