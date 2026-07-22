.PHONY: build check lint plan-fixtures test test-race vet

check:
	mise run check

build:
	mise run build

lint:
	mise run lint:go
	mise run lint:shell

plan-fixtures:
	mise run plan-fixtures

test:
	mise run test

test-race:
	mise run test:race

vet:
	mise run vet
