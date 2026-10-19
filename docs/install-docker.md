# 用 Docker / Compose 部署

> 返回 [README](../README.md) · 飞牛用户请走 [fnOS 应用包](install-fnos.md)

适用：任何装了 Docker 的 Linux 机器（NAS / 树莓派 / 云主机）。

## 方式一：预构建镜像（最省事）

CI 在每次推 `v*` tag 时构建 **linux/amd64 + linux/arm64** 双架构镜像，
**同时推到 Docker Hub 与 GitHub Container Registry**：

```bash
docker run -d --name autodocker \
  --restart unless-stopped \
  -p 17777:17777 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data:/data" \
  -e TZ=Asia/Shanghai \
  -e SELF_CONTAINER=autodocker \
  haihaipypy/autodocker:latest
```

启动后访问 `http://<你的机器IP>:17777`。

> **默认用 Docker Hub**：国内拉取比 `ghcr.io` 快得多，也能直接吃 Docker 守护进程里
> 已配好的加速站。拉不动时换 GHCR：`ghcr.io/haihaipypy/autodocker:latest`，两者内容完全一致。

**默认零配置**：不设密码、不填飞书，起来就能用。要飞书通知或要加登录门，
进「设置」页填即可（存数据库、即时生效）。要一开始就带密码，加一行
`-e ADMIN_PASSWORD='你的密码'`。

> 端口默认 **17777**，刻意避开 8080 / 8000 / 9000 这些几乎必被占用的端口。
> 换端口要两处一起改：`-p 宿主机:容器内` 的两侧，以及 `-e PORT=`。

| 标签 | 含义 |
| --- | --- |
| `latest` | **最近一次打 tag 的正式版本**（跟着 `v*` tag 走，不跟 main） |
| `1.2.3` / `1.2` | 对应 `v1.2.3` 这个 tag，**部署建议钉这个** |
| `main` | main 分支最新构建，紧跟开发进度，可能夹带未完成的改动 |
| `sha-1a2b3c4` | 对应提交，回滚排查用 |

> Docker Hub 匿名拉取按 IP 限流（每 6 小时 100 次），升级频繁时可以 `docker login` 后拉。
> 拉不动就走下面的方式二本地构建，反正就一条 `docker compose up -d --build`。

## 方式二：本地构建（Compose）

```bash
git clone https://github.com/haihaipypy/autodocker.git
cd autodocker
docker compose up -d
```

**不用改任何东西就能起**：`docker-compose.yml` 里 `ADMIN_PASSWORD` 与三个 `FEISHU_*` 一律留空
——不设密码即不启用登录，不填飞书只是不推通知，其余功能照常。
装好后在网页「设置」里补齐这两项即可（存数据库、即时生效、不用重启），
环境变量只作为**首次启动的初值**，数据库里的值优先。

首次启动会扫描一次，并自动把本机所有带 tag 的镜像纳入监控。
访问 `http://<IP>:17777`。

换端口改 compose 里两处：`ports:` 的 `"17777:17777"` 与 `PORT=17777`。

用预构建镜像就把 `build: .` 换成 `image:` 那一行：

```yaml
services:
  autodocker:
    image: haihaipypy/autodocker:1.2.3   # 或 ghcr.io/haihaipypy/autodocker:1.2.3
    # build: .
```

## 关键配置项

| 变量 | 说明 |
| --- | --- |
| `ADMIN_PASSWORD` | 网页访问密码。**默认空 = 不启用登录**；挂了 docker.sock，要暴露到公网就一定设上。之后可在网页「设置」里改，数据库里的值优先 |
| `ADMIN_USER` | 登录用户名，默认 `admin` |
| `SELF_CONTAINER` | 自己的容器名，用于阻止「更新自己」。默认 `autodocker`，改了容器名要同步改这里 |
| `SCAN_INTERVAL` | 巡检间隔秒数，最小 30，`0` 表示只在手动点击时扫描 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 飞书自建应用凭据。作为**首次启动的初值**，之后在网页「设置」里维护，数据库里的值优先 |
| `FEISHU_CHAT_ID` | 接收通知的群 ID，同上 |
| `AUTO_UPDATE` | 发现即自动更新（无人值守）。默认关闭，即「发现 → 通知 → 等人确认」 |
| `WATCH` | 额外监控本机没有运行的镜像，逗号分隔，例如 `nginx:1.25,redis:7.2` |
| `ROLLBACK_ON_FAILURE` | 更新失败是否自动回滚，默认 `true` |
| `KEEP_OLD_CONTAINER` | 更新成功后是否保留旧容器（默认删除）。调试期建议开 |
| `HEALTH_CHECK_WAIT` | 新容器启动后的健康检查观察窗口（秒），默认 20 |
| `REGISTRY_MIRROR` | Docker Hub 加速地址，可填多个、逗号分隔、按序回退，例如 `docker.1ms.run,docker.xuanyuan.me`。留空时按「守护进程 `registry-mirrors` → 内置默认」依次兜底。**只作用于 Docker Hub**，ghcr.io / quay.io / 私有仓库仍直连 |
| `REGISTRY_AUTH` | 私有仓库凭据，形如 `docker.io=user:pass,ghcr.io=ghp_xxx` |
| `REGISTRY_INSECURE` | 允许 http 明文注册表。`127.0.0.1` / `localhost` / `::1` 这类回环地址**不需要打开它**，会自动走 http（与 Docker 守护进程的默认规则一致）；打开后则是**所有**源都改用 http，Docker Hub 与公共加速站会因此整片失败，只在必要时刻临时开 |
| `STATIC_DIR` | 指向 `web/` 目录以覆盖内嵌前端，改前端时免重新编译 |

## 关于容器本身

镜像是「二进制 + ca-certificates + tzdata」三件套：前端已由 `web/embed.go` 的
`go:embed` 编进二进制，**不需要把 `web/` 目录拷进运行阶段**。

- 容器自带 `HEALTHCHECK`，探针打静态首页 `/`，刻意避开需要鉴权的 `/api/*`
  —— 配了密码后 API 一律 401，拿它做探针会让容器永远「不健康」；
- **刻意以 root 运行**：`docker.sock` 的属组 GID 每台机器不同（0 / 998 / 999 都见过），
  写死一个会让部分机器直接起不来；
- 只需要挂两处：`/var/run/docker.sock`（监控与重建的基础）与 `/data`（SQLite 数据库）。

## 本机开发

```bash
# 后端（默认 17777；飞牛应用里由 run.sh 指定为 19999）
go run ./cmd/server

# 前端是纯静态文件，改完刷新浏览器即可；
# 也可以指定 STATIC_DIR 指向 web/ 目录来覆盖内嵌资源：
STATIC_DIR=./web go run ./cmd/server
```

本地构建镜像：

```bash
docker build -t autodocker:dev .
docker run --rm -p 17777:17777 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data:/data" autodocker:dev
```

`.dockerignore` 排掉了 `fnos/`（里面有 13 MB 的 linux 二进制）、`*.fpk`、
`tools/`、`.workbuddy/`，构建上下文只有源码，几秒就能传完。
