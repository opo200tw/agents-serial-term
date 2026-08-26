PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all dist clean test
all:
	go build -o serial-term .
	@ls -la serial-term

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		GOOS=$$os GOARCH=$$arch go build -o dist/serial-term-$$os-$$arch$$ext .; \
	done
	@ls -la dist/

test:
	go test ./...

clean:
	rm -f serial-term
	rm -rf dist
