# 网格原生应用的镜像（与 examples/docker/Dockerfile 同策略：宿主编译好再 COPY）。
#
# 为什么单独一份而不是复用 examples/docker/Dockerfile：
#   微服对照组（launchx-demo:dev）与网格原生组（launchx-mesh-apps:dev）必须各自
#   可独立升级 —— 两套证据链不能因为共用镜像 tag 而互相污染。
#
# 一个镜像装**五个**二进制（bff + 四个 srv-<域>），各 Deployment 用 command 区分：
#   command: ["/app/bin/bff"] / srv-order / srv-points / srv-product / srv-stock
# 刻意**没有 gateway**：入口转发整块交给 Istio Gateway + VirtualService
#（见 README §3），故 cmd/gateway 不存在，这里也不得出现它的路径。
#
# 构建上下文 = 本目录，配合 .dockerignore 排除源码，只送 bin/。
FROM alpine:3.20

# 刻意不装包：minikube 的 daemon 在本机网络下访问不了 docker.io 与 alpine CDN，
# 构建期只要联网就必然 timeout。基础镜像先在宿主拉好再 minikube image load。
#
# 工作目录 = **apps 根**（不是 conf/）：五个 cmd/main.go 都以相对路径
# "conf/<svc>.yaml" 读配置（本机起服时也在 apps 根跑 `go run ./cmd/<svc>`），
# 而 Deployment 把 ConfigMap 经 subPath 挂到 /app/conf/<svc>.yaml —— 两边对齐。
# 对照 micro 的 docker/Dockerfile：那边是 WORKDIR /app/conf + "config.yaml"，
# 因为它的每个服务各占一个目录、配置名统一叫 config.yaml。
WORKDIR /app

# 预建挂载点的父目录：.dockerignore 排除了 conf/，镜像里本没有 /app/conf，
# 而 ConfigMap 是 subPath 挂到 /app/conf/<svc>.yaml 的。kubelet 通常会自动
# 补建父目录，但显式建一次不依赖那个行为，也不联网。
RUN mkdir -p /app/conf

COPY bin/ /app/bin/

# 五个 Deployment 共用本镜像，各自用 command 指定跑哪个；
# 留一个默认（bff，唯一有 HTTP 面、docker run 手测能 curl 的进程）。
ENTRYPOINT ["/app/bin/bff"]
