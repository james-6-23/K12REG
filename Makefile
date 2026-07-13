.PHONY: build build-ui build-all run serve test tidy docker

BINARY := k12reg

build:
	go build -o $(BINARY) ./cmd/server

build-ui:
	cd frontend && npm run build

build-all: build-ui build

serve: build
	./$(BINARY) serve -data ./data -static ./frontend/dist -password $${WEB_PASSWORD:-admin} -addr :$${PORT:-8000}

run: build
	./$(BINARY) -data ./data -count $${COUNT:-1}

tidy:
	go mod tidy

docker:
	docker compose up -d --build
