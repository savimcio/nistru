.PHONY: test test-short race cover lint fuzz e2e ci fmt clean examples-test

test:
	go test ./...

test-short:
	go test -short ./...

race:
	go test -race -count=1 ./...

cover:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...
	go run -tags tools honnef.co/go/tools/cmd/staticcheck ./...

fuzz:
	for pkg in $$(go list ./...); do \
		for fn in $$(go test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
			echo ">> $$pkg $$fn"; go test -run=^$$ -fuzz=^$${fn}$$ -fuzztime=10s $$pkg || exit 1; \
		done; \
	done

e2e:
	go test -tags=e2e -count=1 ./...

examples-test:
	@for mod in examples/plugins/*/; do \
		echo ">>> go test in $$mod"; \
		(cd "$$mod" && go test -race -count=1 ./...) || exit 1; \
	done

ci:
	go vet ./...
	go run -tags tools honnef.co/go/tools/cmd/staticcheck ./...
	go test -short -race -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1
	$(MAKE) examples-test

fmt:
	gofmt -s -w .

clean:
	rm -f coverage.out coverage.html
