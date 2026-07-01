MODULES := transaction eventbus logging configuration outbox outbox/gowaymigrator sqlbus sqlbus/gowaymigrator transaction/integration
CORES := transaction eventbus logging configuration
FMT_DIRS := transaction eventbus logging configuration outbox outbox/gowaymigrator sqlbus sqlbus/gowaymigrator

.PHONY: build vet test fmt tidy zero-dep check release

build:
	@for m in $(MODULES); do echo "== build $$m =="; go -C $$m build ./...; done

vet:
	@for m in $(MODULES); do echo "== vet $$m =="; go -C $$m vet ./...; done

test:
	@for m in $(MODULES); do echo "== test $$m =="; go -C $$m test ./... -count=1; done

fmt:
	gofmt -w $(FMT_DIRS)

tidy:
	@for m in $(MODULES); do echo "== tidy $$m =="; go -C $$m mod tidy; done

# Assert the leaf cores carry no third-party dependency.
zero-dep:
	@for m in $(CORES); do \
		if grep -qE '^[[:space:]]*require' $$m/go.mod; then \
			echo "ERROR: $$m/go.mod must have no require directive"; exit 1; \
		fi; \
		echo "$$m: zero-dependency core OK"; \
	done

check: zero-dep build vet test
	@test -z "$$(gofmt -l $(FMT_DIRS))" || (echo "gofmt: files need formatting:"; gofmt -l $(FMT_DIRS); exit 1)

# Release the whole family in lockstep, e.g. make release V=v0.1.0
release:
	@test -n "$(V)" || (echo "usage: make release V=v0.1.0"; exit 1)
	./scripts/release.sh $(V)
