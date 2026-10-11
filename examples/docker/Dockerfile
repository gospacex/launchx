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
#   3. **一个镜像装全部二进制**，各 Deployment 用 command 区分 ——
#      这是「同一份二进制跑多种形态」最硬的证据形式：镜像 id 完全相同。
#      六个进程由 examples/micro/scripts/build-bin.sh 产出，名字是
#      demo-gateway / demo-bff / demo-product / demo-order / demo-stock /
#      demo-points（**服务名**，与目录名 srv-* 不是一回事）。
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
# 工作目录 = config.yaml 所在目录：六个 cmd/main.go 都以相对路径 "config.yaml"
# 读配置（Bootstrap("config.yaml")），ConfigMap 就挂到这个路径上。
# ⚠️ 镜像内**没有** config.yaml：下面只 COPY bin/，配置一律由 Deployment 的
# volumeMounts 从 ConfigMap 挂进来（subPath）。二进制与配置缺一不可 ——
# 少了配置，进程在 /app/conf 下找不到 config.yaml，六个 Deployment 全起不来。
WORKDIR /app/conf

# bin/ 是**本地产物、已 gitignore**（.gitignore 的 `examples/**/bin/`）：
# clone 下来这个目录是空的，docker build 会直接报
# `COPY failed: no source files were specified`。构建镜像前必须先跑：
#   bash examples/micro/scripts/build-bin.sh
# （默认 GOOS=linux GOARCH=amd64，产物落在 examples/docker/bin/。）
COPY bin/ /app/bin/

# 六个 Deployment 共用本镜像，各自用 command 指定跑哪个（与
# examples/micro/scripts/build-bin.sh:29-36 的 SPECS 逐条对应）：
#   command: ["/app/bin/demo-gateway"]  # 8080
#   command: ["/app/bin/demo-bff"]      # 8081
#   command: ["/app/bin/demo-product"]  # 9021 / 采集口 9033
#   command: ["/app/bin/demo-order"]    # 9022 / 采集口 9031
#   command: ["/app/bin/demo-stock"]    # 9023 / 采集口 9034
#   command: ["/app/bin/demo-points"]   # 9024 / 采集口 9032
# ⚠️ `demo-srv` 已不再产出：它是四域拆分**之前**的单域旧样例（9013），
# build-bin.sh 的 SPECS 里没有它，写进 command 会 `no such file or directory`。
# 留一个默认（gateway）便于 docker run 手测。
ENTRYPOINT ["/app/bin/demo-gateway"]

# 以非 root 运行（2026-10-10 加固）：镜像侧把身份定死，免得「到底是不是 root」
# 完全取决于 Deployment 有没有写 securityContext —— 两处不一致时，运行时身份
# 就成了悬案。与 examples/micro/deploy/micro.yaml 的 Pod 级
# `runAsNonRoot: true / runAsUser: 65534` 同一口径（65534 = nobody）。
#
# ⚠️ 上这条时必须一起验证 `readOnlyRootFilesystem: true` 仍能正常启动：
# 两者叠加后进程对 /app 下一切路径都不可写（mesh 组已用同一套镜像 + 65534
# 验证可行，只读根下只需给 /tmp 挂一块 emptyDir）。若某个进程确实要写盘，
# 症状是启动期 `permission denied` 或 `read-only file system`，此时应改进程
# 而不是撤掉这两条。
USER 65534:65534
