test:
	@ go vet ./...
	@ go run honnef.co/go/tools/cmd/staticcheck@latest $(shell go list ./... | grep -v /internal/pogo)
	@ go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix -test ./...
	@ go test -race ./...

precommit: test

# Regenerate pgdomino's database client after changing a migration
pogo:
	@ go tool migrate --dir pgdomino/internal/migrate --table domino_migrate --db "$(DATABASE_URL)" up
	@ go tool pogo --db "$(DATABASE_URL)" --dir pgdomino/internal/pogo
	@ rm -rf pgdomino/internal/pogo/dominomigrate

release: VERSION := $(shell awk '/[0-9]+\.[0-9]+\.[0-9]+/ {print $$2; exit}' Changelog.md)
release: test
	@ go mod tidy
	@ test -n "$(VERSION)" || (echo "Unable to read the version." && false)
	@ test -z "`git tag -l v$(VERSION)`" || (echo "Aborting because the v$(VERSION) tag already exists." && false)
	@ test -z "`git status --porcelain | grep -vE 'M (Changelog\.md)'`" || (echo "Aborting from uncommitted changes." && false)
	@ test -n "`git status --porcelain | grep -v 'M (Changelog\.md)'`" || (echo "Changelog.md must have changes" && false)
	@ git commit -am "Release v$(VERSION)"
	@ git tag "v$(VERSION)"
	@ git push origin main "v$(VERSION)"
	@ go run github.com/cli/cli/v2/cmd/gh@latest release create --generate-notes "v$(VERSION)"
