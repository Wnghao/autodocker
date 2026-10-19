# 发布流程

> 返回 [README](../README.md)

一次发版包含两份产物，**版本号必须一致**：

| 产物 | 去处 | 版本号来源 |
| --- | --- | --- |
| Docker 镜像 `ghcr.io/haihaipypy/autodocker` | GitHub Container Registry | `git tag vX.Y.Z` |
| 飞牛应用包 `Autodocker.fpk` | GitHub Releases | `fnos/manifest` 的 `version` |

## 一、改版本号

`fnos/manifest` 三处一起改：

| 位置 | 用途 |
| --- | --- |
| `version` | 飞牛应用中心显示的版本 |
| `changelog` | 更新说明，**单行**，最新的写在最前，旧的往后接 |
| `platform` / `resource` 等 | 与版本无关，一般不动 |

`changelog` 是单行字符串，写法是在同一行里按版本倒序拼接：

```
changelog = 1.2.3 本次…；1.2.2 图标去掉白色底…；1.2.1 界面重做…
```

同时把 README 与 docs 里出现的旧版本号（镜像标签示例等）一并更新。

## 二、本地校验

```bash
go test ./... && go vet ./... && gofmt -l .
node --check web/app.js
```

`gofmt -l .` 应该什么都不输出。

## 三、打飞牛应用包

```bash
rm -f fnos/app/bin/autodocker      # 必做，见下方说明
./build-fpk.sh                     # x86
./build-fpk.sh arm64               # ARM
```

> **编译前先删旧二进制。** `go build -o` 在目标文件已存在且 build ID 相同时会跳过写入，
> 结果打出来的包还是上一版的程序。

产物是项目根目录的 `Autodocker.fpk`。解包核对一遍：

```bash
tar -tzf Autodocker.fpk | head -30
```

`.fpk` **不进 git**（体积 6 MB 左右），只作为 Release 附件。

## 四、推送代码

```bash
git add -A
git commit -m "release: v1.2.3"
git push origin main
```

先推 main，确认 Actions 里的 `Build & Push Image` 跑通了，再打 tag。
反过来的话 `latest` 会先指向一个还没验证过的版本。

## 五、打 tag 触发镜像构建

```bash
git tag v1.2.3
git push origin v1.2.3
```

| 触发 | 产物标签 |
| --- | --- |
| push 到 `main` | `main`、`sha-xxxxxxx` |
| push tag `v1.2.3` | `1.2.3`、`1.2`、**`latest`**、`sha-xxxxxxx` |
| Pull Request | 只构建校验，不推送 |
| Actions 页面手动触发 | 同 main |

> `latest` 只在打 tag 时移动。分支构建不抢这个名字，否则 main 上的半成品会直接
> 成为用户拉到的「最新版」。

会构建 `linux/amd64` + `linux/arm64` 双架构（飞牛有 x86 也有 ARM 机型），
推到 `ghcr.io/haihaipypy/autodocker`。不用配任何 secret —— 用的是仓库自带的
`GITHUB_TOKEN`，workflow 里已声明 `packages: write`。

> **第一次发布后必须手动改一次可见性。**
> GHCR 新建的包默认是 **private**，别人 `docker pull` 会报 `denied`。
> 打开 `https://github.com/haihaipypy?tab=packages` → 点进 `autodocker` →
> 右下角 `Package settings` → `Change visibility` → 选 `Public` 并确认。
> 之后发布的版本会沿用这个设置，只需改一次。

## 六、建 Release 并上传 fpk

```bash
gh release create v1.2.3 Autodocker.fpk \
  --title "v1.2.3" \
  --notes-file /tmp/notes.md
```

或者网页操作：仓库 → Releases → Draft a new release → 选 tag `v1.2.3` →
把 `Autodocker.fpk` 拖进去。

Release 说明按「新增 / 改进 / 修复」三段写，把 `manifest` 的 changelog 展开成可读条目。

## 七、可选：同时发 Docker Hub

GHCR 免申请、和仓库绑在一起，公开后拉取不限速，一般不需要 Docker Hub。
确实要发就在 workflow 里加两步：

```yaml
      - name: 登录 Docker Hub
        if: github.event_name != 'pull_request'
        uses: docker/login-action@v3
        with:
          username: ${{ secrets.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - name: 构建并推送（含 Docker Hub）
        uses: docker/build-push-action@v6
        with:
          # ...
```

并把 `metadata-action` 的 `images:` 改成两行（换行分隔）：

```yaml
          images: |
            ghcr.io/${{ github.repository }}
            ${{ secrets.DOCKERHUB_USERNAME }}/autodocker
```

secrets 在仓库 Settings → Secrets and variables → Actions 里加
`DOCKERHUB_USERNAME` 与 `DOCKERHUB_TOKEN`（Docker Hub 后台生成的 access token，
不是登录密码）。

## 八、发版前 checklist

- [ ] `fnos/manifest` 的 `version` 与即将打的 tag 一致
- [ ] `changelog` 已加本次条目，写的是用户能感知的变化
- [ ] 删过 `fnos/app/bin/autodocker` 再打包
- [ ] `go test ./...` 与 `go vet ./...` 通过
- [ ] main 推送后 Actions 通过
- [ ] tag 推送后双架构镜像推送成功
- [ ] GHCR 包可见性为 Public（首次）
- [ ] Release 附上了 `Autodocker.fpk`，且解包能核对到新版本号
