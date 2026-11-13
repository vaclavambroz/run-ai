GO ?= go
PYTHON ?= python3
export PYTHONDONTWRITEBYTECODE := 1

.PHONY: all build test test-go test-packaging lint clean

all: build

build:
	mkdir -p build
	$(GO) build -trimpath -buildvcs=false -o build/run_ai ./cmd/run_ai

test: test-go test-packaging

test-go:
	$(GO) test ./...

test-packaging:
	$(PYTHON) -m unittest discover -s packaging/tests -p 'test_*.py' -v

lint:
	@unformatted="$$(gofmt -l .)"; test -z "$$unformatted" || { printf '%s\n' "$$unformatted" >&2; exit 1; }
	$(GO) vet ./...
	@find packaging -type f -name '*.sh' -exec sh -n {} \;
	$(PYTHON) -c 'import ast,pathlib; [ast.parse(p.read_text(), filename=str(p)) for p in pathlib.Path("packaging").rglob("*.py")]'
	$(PYTHON) -m json.tool packaging/release/manifest.schema.json >/dev/null
	$(PYTHON) -m json.tool packaging/release/release-input.example.json >/dev/null
	$(PYTHON) -m json.tool packaging/wsl/wsl.json >/dev/null
	$(PYTHON) -m json.tool packaging/wsl/profile.schema.json >/dev/null

clean:
	rm -rf build dist
