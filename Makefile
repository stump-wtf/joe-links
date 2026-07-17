.PHONY: build run migrate css clean tidy swagger dev dev-stop docker-build docker-up docker-down ext-safari

BINARY := joe-links

build: css
	go build -o bin/$(BINARY) ./cmd/joe-links

run: css
	go run ./cmd/joe-links serve

migrate:
	go run ./cmd/joe-links migrate

css:
	npm run build

clean:
	rm -rf bin/ web/static/css/app.css node_modules/

tidy:
	go mod tidy

# swag is pinned to the exact version go.mod requires — the version that
# generated the committed docs/swagger/. Keep SWAG_VERSION in sync with the
# github.com/swaggo/swag entry in go.mod (internal/packaging/ci_test.go
# enforces the match). See issue #213.
SWAG_VERSION := v1.16.6

swagger:
	go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION) init -g internal/api/main_annotations.go -o docs/swagger --outputTypes json,yaml,go --parseDependency --parseInternal

dev:
	docker compose -f docker-compose.dev.yml up -d
	sudo go run ./cmd/joe-links serve

dev-stop:
	docker compose -f docker-compose.dev.yml down

docker-build:
	docker build -t joe-links .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

# Open the committed Safari extension Xcode project. It references the Web
# Extension source at integrations/extension/ directly, so there is no
# safari-web-extension-converter step — build in Xcode (Cmd+B), then enable
# the extension in Safari → Settings → Extensions.
ext-safari:
	open integrations/apple/joe-links.xcodeproj

.DEFAULT_GOAL := build
