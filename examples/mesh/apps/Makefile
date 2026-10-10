# 网格原生应用的构建入口。
#
# 独立 module ⇒ 根包 `go build ./...` 覆盖不到本目录，构建与测试必须走这里；
# 漏掉这一步就等于「代码坏了但门禁全绿」。CI 里要显式调 `make build test`。

DOCKER   ?= /Applications/Docker.app/Contents/Resources/bin/docker
IMAGE    ?= launchx-mesh-apps:dev
GO       ?= go
# **五个**进程：bff + 四个 srv-<域>。刻意没有 gateway ——
# 入口转发整块交给 Istio Gateway + VirtualService（见 README §3 与 §4.1），
# 所以这里既没有 cmd/gateway，也不该把它列进来：多一个不存在的目录会让
# 整个 build 循环在第一轮就 exit 1（这正是它曾经的样子）。
SERVICES := bff srv-order srv-points srv-product srv-stock

.PHONY: all build test vet fmt tidy image clean

all: build

build:
	@mkdir -p bin
	@for svc in $(SERVICES); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o bin/$$svc ./cmd/$$svc || exit 1; \
		echo "  built bin/$$svc"; \
	done

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "need gofmt: $$out" >&2; exit 1; fi

tidy:
	$(GO) mod tidy

image: build
	@command -v minikube >/dev/null 2>&1 || { echo "missing minikube" >&2; exit 1; }
	@set -e; eval "$$(minikube docker-env)"; \
		$(DOCKER) build -f Dockerfile -t $(IMAGE) . ; \
		echo "  built $(IMAGE)"
	@minikube image ls | grep "$(IMAGE)" || true

clean:
	rm -rf bin
