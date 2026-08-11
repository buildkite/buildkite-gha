.PHONY: build check lint test test-race vet

check:
	mise run check

build:
	mise run build

lint:
	mise run lint:go
	mise run lint:shell

test:
	mise run test

test-race:
	mise run test:race

vet:
	mise run vet
