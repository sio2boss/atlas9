VERSION := $(shell ./tools/version.sh)

.PHONY: all test tests release homebrew update-version

update-version:
	@sed -i '' 's/"v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*"/"$(VERSION)"/' cmd/atlas9/main.go

all: update-version
	go mod tidy
	go build -o atlas9 cmd/atlas9/main.go

test:
	go test ./...

tests: test
release: update-version
	gox -osarch="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 freebsd/amd64 openbsd/amd64 netbsd/amd64" -output="release/atlas9_{{.OS}}_{{.Arch}}" ./cmd/atlas9
	echo "Upload to github releases and then run make homebrew"

homebrew:
	@echo "Updating Homebrew formula for version $(VERSION)..."
	./tools/update_homebrew_formula.sh $(VERSION)