# 脚本型监控项（hook）的辅助容器镜像。
#
# update.sh 这类包装脚本要同时用 bash、curl、docker、compose、git、python3，
# 而现成的 docker:*-cli 镜像只有 docker CLI，跑一半就 command not found。
# 所以自己拼一个，构建完把镜像名填到设置里的 HOOK_IMAGE。
#
#   docker build --network host -t autodocker-hook-runner:local tools/hook-runner
#
# --network host 是给 apk 用的：默认 bridge 网络下容器可能出不了网。
# PROXY 只在构建期生效，不会留在镜像里 —— 脚本自己知道该用哪个代理。

FROM alpine:3.20

ARG PROXY=http://127.0.0.1:7890
ENV http_proxy=${PROXY} https_proxy=${PROXY} \
    HTTP_PROXY=${PROXY} HTTPS_PROXY=${PROXY}

RUN apk add --no-cache \
    bash curl docker-cli docker-cli-compose git python3 \
    tar coreutils grep sed gawk ca-certificates tzdata openssh-client

ENV http_proxy= https_proxy= HTTP_PROXY= HTTPS_PROXY=

# 项目目录挂在容器里属主仍是宿主机的 uid，git 会按「可疑所有权」拒绝操作。
RUN git config --global --add safe.directory '*'
