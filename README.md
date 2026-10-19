# Autodocker

**Docker 镜像更新监测 · 飞书确认 · 一键自动重建**

[![Release](https://img.shields.io/github/v/release/haihaipypy/autodocker?label=release&color=blue)](https://github.com/haihaipypy/autodocker/releases/latest)
[![Docker Pulls](https://img.shields.io/docker/pulls/haihaipypy/autodocker)](https://hub.docker.com/r/haihaipypy/autodocker)
[![License](https://img.shields.io/github/license/haihaipypy/autodocker)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/haihaipypy/autodocker)](go.mod)

监测本机容器使用的镜像是否有了新版本，把提醒推到飞书；你在群里点一下「立即更新」按钮，
服务就会拉取新镜像并按原配置重建容器 —— 挂载、端口、网络、环境变量全部保留，
失败自动回滚。本机没人使用的镜像也一并列出来，随时可以一键清理掉。

![Autodocker](docs/images/cover.png)

> 功能上相当于把 [vigil](https://github.com/jingyuan9527/vigil) 的「检测 + 多渠道通知」与
> [dockerCopilot](https://github.com/onlyLTY/dockerCopilot) 的「一键更新容器」合到一起，
> 中间补上它们都没有的那一环：**在 IM 消息里点按钮直接完成更新**。

---

## 它解决什么问题

| 场景 | 传统做法 | Autodocker |
| --- | --- | --- |
| 想知道镜像有没有更新 | 手动 `docker pull` 比对，或装一个 Watchtower 全自动更新（不放心） | 定时巡检，发现更新推卡片到飞书 |
| 决定要不要更新 | 登录 NAS / SSH 上去点 | 在飞书消息里点「立即更新」 |
| 更新容器 | 手写 `docker run`，稍不留神漏掉挂载或环境变量 | 自动 inspect 原配置后重建，字段零丢失 |
| 更新把服务搞挂了 | 手动找回原配置重新起 | 健康检查不通过自动回滚 |
| 磁盘被旧镜像占满 | 一个个 `docker rmi`，还怕删错正在用的 | 「镜像」页标明使用状态，一键清理无 TAG / 未使用 |

## 特性

- **容器与镜像分开管理**。容器页讲「在跑的东西该不该更新」，镜像页讲「磁盘上有什么、
  能不能清」，不混在一张表里。
- **两种检测模式自动判定**。浮动 tag（`latest`）比摘要；版本号 tag（`1.25`、`v3.9`）
  额外巡检仓库 tag 列表，出现更高版本时提示「可选更新」。
- **按钮只在该出现时渲染**。「已是最新」「未知」「已忽略」不摆灰按钮；
  检测失败的原因写在容器名下方，比一个「未知」徽章有用。
- **两个忽略粒度**。卡片上「忽略本版本」只放行这一次远端内容，作者推了新版本照常提醒；
  容器页「忽略」才是整条监控项不再检测。
- **深色玻璃拟态界面 + 深浅主题切换**，不依赖任何前端构建工具，一个二进制带全部静态资源。
- **网页与飞书双向同步**。飞书里点按钮绕开了页面，页面会自己回查任务状态并重载结果。

## 安装

三种方式任选，详细步骤见文档：

### 1. 飞牛 fnOS 应用包（推荐）

```bash
./build-fpk.sh            # ARM 设备用 ./build-fpk.sh arm64
# 产物 Autodocker.fpk，在应用中心 → 手动安装里选中它
```

装完桌面出现「Docker 更新助手」，也可直接访问 `http://<NAS_IP>:19999`。
安装向导只问巡检间隔与镜像加速地址，飞书凭据装完在「设置」页填。

→ [飞牛安装与排障](docs/install-fnos.md)

### 2. Docker / Compose

```bash
git clone https://github.com/haihaipypy/autodocker.git
cd autodocker
docker compose up -d        # 访问 http://<IP>:17777
```

**零配置可用**：默认端口 **17777**（避开 8080 这类几乎必被占用的端口），
默认**不设访问密码、不填飞书**——起来就能在网页里看容器与镜像。
要飞书通知或要加一道登录门，进「设置」里填即可，存数据库、即时生效、不用重启。

→ [Docker / Compose 部署与全部环境变量](docs/install-docker.md)

### 3. 直接拉预构建镜像

```bash
docker run -d --name autodocker --restart unless-stopped \
  -p 17777:17777 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data:/data" \
  -e TZ=Asia/Shanghai -e SELF_CONTAINER=autodocker \
  haihaipypy/autodocker:latest
```

**默认走 Docker Hub**（`haihaipypy/autodocker`）——国内拉取比 `ghcr.io` 快得多，
也正好能用上 Docker 守护进程已配的加速站。GitHub Container Registry 同步发布，
拉不动 Docker Hub 时换这个：

```bash
docker pull ghcr.io/haihaipypy/autodocker:latest
```

要一开始就带密码，加一行 `-e ADMIN_PASSWORD='你的密码'`（之后在网页里改也可以，数据库里的值优先）。
换端口记得容器内 `PORT` 一起改：`-p 18000:18000 -e PORT=18000`。

双架构 `linux/amd64` + `linux/arm64`。`latest` 跟着 `v*` tag 走，部署建议钉版本号
（如 `:1.2.3`）。

## 使用

![界面](docs/images/ui-dark.png)

定时巡检 → 发现新版本推飞书卡片 → 点「立即更新」→ 拉镜像、按原配置重建、健康检查
→ 通过则删旧容器，不通过自动回滚。

「可用」列写的是**仓库里更高的版本号**；没有更高版本号但同一 tag 被重新推送过，
写「内容已更新」。镜像页每行第一行镜像名、第二行镜像 ID，跟 `docker images` 一眼对上。

→ [完整使用说明](docs/usage.md)：容器/镜像两列表的边界、两个忽略粒度、检测模式、
悬空镜像的命名规则、清理策略、主题与自动刷新

## 飞书配置

**必须是企业自建应用，群自定义机器人（webhook）收不到按钮点击。**

六步：建应用 → 开机器人能力 → 申请 `im:message:send_as_bot`（应用身份）→
在「**回调配置**」标签页用长连接订阅 `card.action.trigger` → **发布新版本** →
把机器人拉进群，最后在设置页填 App ID / Secret / chat_id。

两个最容易漏的点：回调加在旁边的「事件配置」里不生效；权限或回调改完不发布新版本，
点按钮报 `200340` 但卡片照收。

→ [飞书配置逐图步骤](docs/feishu.md)

## 安全

- 挂载 `/var/run/docker.sock` **等同于宿主机 root 权限**，请只在可信内网使用。
- 访问密码**默认空即不启用登录**。要让端口离开内网，务必在「设置」里配上密码。
- 服务会拒绝更新自身容器（`SELF_CONTAINER` + 容器内 hostname 双重判断），
  也会拒绝更新带 `--rm` 的容器（停止即被回收，没法回滚）。

→ [安全须知与已知边界](docs/faq.md)

## 技术简述

- 纯 Go 单二进制，前端零构建（`go:embed`），SQLite 用 `modernc.org/sqlite`（免 CGO）。
- **不依赖 Docker 官方 SDK**：直接封装 Engine REST API，`HostConfig` 用
  `map[string]any` 原样透传 —— 更适合「原样还原用户配置」这个需求。
- 飞书走 **WebSocket 长连接**收回调，免公网、免备案、免反向代理；卡片用 2.0 结构。
- 注册表加速三层兜底：`用户设置 → 守护进程 registry-mirrors → 内置默认
  docker.1ms.run, docker.xuanyuan.me`，且只作用于 Docker Hub。
- 重建时主动修正几处照抄 `inspect` 会出错的字段（镜像引用、Hostname、内置网络、
  网络端点、匿名卷）。

→ [技术实现、目录结构与测试](docs/architecture.md)

## 发布与版本

镜像 tag 与飞牛应用版本号保持一致（`v1.2.3` ↔ `manifest` 的 `1.2.3`），
`.fpk` 作为 Release 附件提供。

→ [发布流程](docs/release.md)

## 文档索引

| 文档 | 内容 |
| --- | --- |
| [install-fnos.md](docs/install-fnos.md) | 飞牛应用打包、安装、运行身份、排障 |
| [install-docker.md](docs/install-docker.md) | Docker / Compose 部署、全部环境变量、本地开发 |
| [feishu.md](docs/feishu.md) | 飞书自建应用配置全流程（含截图） |
| [usage.md](docs/usage.md) | 两个列表的边界、忽略粒度、检测模式、界面 |
| [architecture.md](docs/architecture.md) | 技术选型、重建保真规则、存储坑位、测试 |
| [faq.md](docs/faq.md) | 安全须知、已知边界、常见故障排查 |
| [release.md](docs/release.md) | 版本号、CI、GHCR、Release 上传 |

## 许可与致谢

MIT，见 [LICENSE](LICENSE)。

本项目为**独立实现**：`go.mod` 只依赖飞书 SDK 与纯 Go SQLite 驱动，业务代码全部自研。
[vigil](https://github.com/jingyuan9527/vigil)（AGPL-3.0）与
[dockerCopilot](https://github.com/onlyLTY/dockerCopilot)（AGPL-3.0）仅作**思路参考**，
未复制其代码。如果你想找更成熟的多渠道通知方案，建议直接去看它们。

---

作者：**无辣** · [blog.1day.vip](https://blog.1day.vip)
