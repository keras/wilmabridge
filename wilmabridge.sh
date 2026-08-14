#!/bin/bash


. <(sed 's/^/export /' .env)

go run ./cmd/wilmabridge "$@"
