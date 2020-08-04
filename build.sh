#!/bin/sh

CC='x86_64-linux-musl-gcc' CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildmode 'pie' -a -v -tags 'netgo osusergo static_build' -ldflags '-s -w -extldflags "-static"'