set ignore-comments := true

default:
  @just --list

# Run all checks that CI runs.
check: lint actionlint zizmor test race bench-smoke vet tidy-check

fmt:
  golangci-lint fmt

lint:
  golangci-lint run

test pattern="":
  go test -count=1 -shuffle=on {{ if pattern == "" { "" } else { "-run '" + pattern + "'" } }} ./...

race:
  go test -race ./...

# Run each benchmark one time, so a broken benchmark fails the check. This
# measures nothing.
bench-smoke:
  go test -run '^$' -bench . -benchtime=1x ./...

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
