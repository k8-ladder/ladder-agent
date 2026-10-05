IMAGE   ?= ladder-agent:dev
CLUSTER ?= dev
VERSION ?= $(shell date -u +%Y%m%d%H%M%S)

.PHONY: build test vet image import deploy up logs fakebe run reset

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

import: image
	k3d image import $(IMAGE) -c $(CLUSTER)

deploy:
	kubectl apply -k deploy
	kubectl -n ladder rollout restart deploy/ladder-agent
	kubectl -n ladder rollout status deploy/ladder-agent

# Build, import into k3d and (re)start the agent.
up: import deploy

logs:
	kubectl -n ladder logs -f deploy/ladder-agent

fakebe:
	go run ./cmd/fakebe

# Run the agent on the laptop against the current kube context. Local-mode
# setup then needs: kubectl -n ladder-git port-forward svc/gitea-http 3000
run:
	LADDER_BE_URL=ws://localhost:9000/agent/connect LADDER_AGENT_TOKEN=dev-token \
	LADDER_GITEA_URL=http://localhost:3000 go run ./cmd/agent

# Remove everything setup created, to run it again on the same cluster.
reset:
	hack/reset.sh
