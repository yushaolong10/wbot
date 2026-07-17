.PHONY: dev build test web desktop-native clean
dev:
	go run ./cmd/wbot-server
web:
	cd web && npm ci && npm run build
build: web
	mkdir -p bin
	go build -o bin/wbot-server ./cmd/wbot-server
	go build -o bin/wbot-desktop ./cmd/wbot-desktop
desktop-native: web
	mkdir -p bin
	@if [ "$$(uname -s)" = "Darwin" ]; then \
		CGO_LDFLAGS="$${CGO_LDFLAGS:+$$CGO_LDFLAGS }-framework UniformTypeIdentifiers" \
		go build -tags "wails,desktop,production" -o bin/wbot-wails ./cmd/wbot-wails && \
		codesign --force --sign - bin/wbot-wails; \
	else \
		go build -tags "wails,desktop,production" -o bin/wbot-wails ./cmd/wbot-wails; \
	fi
test:
	go test ./...
	go vet ./...
	cd web && npm run build
clean:
	rm -rf bin web/dist
