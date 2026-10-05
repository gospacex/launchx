# =============================================================================
# launchx demo 运行镜像（微服 / 网格两类案例共用）
# -----------------------------------------------------------------------------
# 设计要点（都是踩过的坑，别改回去）：
#
#   1. **二进制在宿主交叉编译好再 COPY**，不在镜像里跑 go build：
#      容器内 build 会重新拉 3GB 依赖（26 条 replace 全是本地路径，go.mod 里
#      的 require 仍需联网取），构建时间和失败面都不可控；而且容器内 build
#      要求构建上下文是 hubx 仓库根（replace 指向 ../webx 等兄弟 SDK），
#      那个目录下挂着 mongodbx/data、objectx/data 等数据目录，tar 一遍极慢。
#      宿主编译好之后，**上下文只需本目录**（内含 bin/），秒级完成：
#        docker build -f <此文件> -t launchx-demo:dev examples/docker
#      **宿主架构实测是 x86_64**（uname -m = x86_64，别照 Apple Silicon 猜），
#      minikube(docker driver) 同为 amd64 → GOOS=linux GOARCH=amd64。
#      GOARCH 写错的表现是容器内 `exec format error`，见 README 排查表。
#
#   3. **一个镜像装三个二进制**，三个 Deployment 用 command 区分 ——
#      这是「网格复用同一份二进制」最硬的证据形式：镜像 id 完全相同。
#
#   4. 不用 scratch：业务要发 HTTPS 出向、要读时区（DSN 的 loc=Local）。
# =============================================================================
FROM alpine:3.20

# 刻意**不装任何包**（无 apk add）：
#   minikube 的 docker daemon 在这个网络环境下访问不了 docker.io 与
#   dl-cdn.alpinelinux.org，构建期只要联网就必然 timeout；基础镜像也是先在宿主
#   拉好再 `minikube image load` 灌进去的。demo 只连明文中间件（mysql/kafka/
#   jaeger/consul），不需要根证书与 tzdata；真要出向 HTTPS 时再按需 COPY 证书。
#
# 工作目录 = config.yaml 所在目录：三个 cmd/main.go 都以相对路径 "config.yaml"
# 读配置（Bootstrap("config.yaml")），ConfigMap 就挂到这个路径上。
WORKDIR /app/conf

COPY bin/ /app/bin/

# 三个 Deployment 共用本镜像，各自用 command 指定跑哪个：
#   command: ["/app/bin/demo-gateway"] / demo-bff / demo-srv
# 留一个默认（gateway）便于 docker run 手测。
ENTRYPOINT ["/app/bin/demo-gateway"]
