.PHONY: check plan-fixtures test vet

check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go test ./...
	go vet ./...
	$(MAKE) plan-fixtures

plan-fixtures:
	python3 testdata/plans/validate.py

test:
	go test ./...

vet:
	go vet ./...
