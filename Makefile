VERSION := $(shell ./tools/version.sh)

.PHONY: all test tests release homebrew update-version

all:
	go build -trimpath ./cmd/atlas9

update-version:
	@sed -i '' 's/"v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*"/"$(VERSION)"/' cmd/atlas9/main.go

test:
	go test ./...

tests: test
release: update-version
	go mod tidy
	@mkdir -p release
	gox -osarch="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 freebsd/amd64 openbsd/amd64 netbsd/amd64" -gcflags="all=-trimpath=$(CURDIR)" -asmflags="all=-trimpath=$(CURDIR)" -output="release/atlas9_{{.OS}}_{{.Arch}}" ./cmd/atlas9
	@# Create tar.gz archives for Unix platforms (rename binary to 'atlas9' inside archive)
	@for f in release/atlas9_linux_* release/atlas9_darwin_* release/atlas9_freebsd_* release/atlas9_openbsd_* release/atlas9_netbsd_*; do \
		if [ -f "$$f" ]; then \
			mv "$$f" release/atlas9; \
			tar -C release -cvzf "$$f.tar.gz" atlas9; \
			rm -f release/atlas9; \
		fi; \
	done
	@# Create zip archives for Windows
	@for f in release/atlas9_windows_*; do \
		if [ -f "$$f" ]; then \
			mv "$$f" release/atlas9.exe; \
			zip -j "$$f.zip" release/atlas9.exe; \
			rm -f release/atlas9.exe; \
		fi; \
	done
	@echo "Upload to github releases and then run make homebrew"

homebrew:
	@echo "Updating Homebrew formula for version $(VERSION)..."
	./tools/update_homebrew_formula.sh $(VERSION)