COMPOSE := $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo docker-compose)

.PHONY: data up down build proto test ingest embed eval serve load models demo

data:            ## download the WANDS dataset
	./scripts/download.sh

up:              ## start Postgres + Grafana/Prometheus/Tempo/Loki
	$(COMPOSE) -f deploy/compose.yaml up -d --wait

down:
	$(COMPOSE) -f deploy/compose.yaml down

models:          ## pull the free local models
	ollama pull nomic-embed-text
	ollama pull llama3.2:3b

build:
	go build -o bin/aisle ./cmd/aisle

proto:
	protoc -I proto --go_out=gen --go_opt=paths=source_relative \
	  --go-grpc_out=gen --go-grpc_opt=paths=source_relative proto/aisle/v1/search.proto

test:
	go vet ./... && go test -race ./...

ingest: build
	./bin/aisle ingest

embed: build
	./bin/aisle embed

eval: build
	./bin/aisle eval

serve: build
	./bin/aisle serve

load: build
	./bin/aisle load --rps 5

demo: data up models ingest embed eval  ## everything, from nothing to scores
