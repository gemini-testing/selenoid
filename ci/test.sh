#!/bin/bash

set -e

export GO111MODULE="on"
CGO_ENABLED=1 go test -tags 's3 metadata' -v -race -coverprofile=coverage.txt -covermode=atomic -coverpkg github.com/aerokube/selenoid,github.com/aerokube/selenoid/session,github.com/aerokube/selenoid/config,github.com/aerokube/selenoid/protect,github.com/aerokube/selenoid/service,github.com/aerokube/selenoid/upload,github.com/aerokube/selenoid/info,github.com/aerokube/selenoid/jsonerror .
CGO_ENABLED=1 go test -tags 's3 metadata' -v -race -coverprofile=coverage.txt -covermode=atomic -coverpkg github.com/aerokube/selenoid/wsdriver ./wsdriver

go install golang.org/x/vuln/cmd/govulncheck@latest
go install github.com/tk-l2002/govulncheck-wrapper@latest
export IGNORE_GOVULNCHECK=.govulncheck-ignore.yaml
"$(go env GOPATH)"/bin/govulncheck-wrapper -tags production ./...
