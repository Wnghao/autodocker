# Webhook（群机器人）通知

> 返回 [README](../README.md)

Webhook 是最省事的一条通知渠道：把一个接收地址填进设置页，发现镜像更新时
服务就往这个地址 POST 一条 JSON。典型用法是**飞书群自定义机器人**，也适用于
n8n、自建脚本这类能收 HTTP 请求的接收端。

> ⚠️ webhook 只能**单向推送**。消息上不会有「立即更新」按钮，点击也收不到。
> 需要「在聊天里点一下直接重建容器」，请配置 [飞书自建应用](feishu.md) 或 [Telegram](telegram.md)；
> 几个渠道可以同时启用，webhook 和它们互不影响。

## 飞书群机器人 · 四步

1. 飞书里打开目标群 → 右上角 **设置** → **群机器人** → **添加机器人** → 选 **自定义机器人**。
2. 建好后飞书会给一个地址：

   ```
   https://open.feishu.cn/open-apis/bot/v2/hook/xxxxxxxx-xxxx-xxxx
   ```

   把它填进 **设置 → 通知渠道 → Webhook → Webhook 地址**。

3. **安全设置**（可选）：
   - 选了「**签名校验**」：把生成的密钥填到 **签名密钥**；
   - 选了「**自定义关键词**」：关键词要能在通知正文里出现，例如 `AutoDocker`
     （通知标题里带容器名，正文里带镜像名，一般不缺可用的词）。

4. 点页面底部 **保存设置**，再点 **发送测试消息**。群里收到即说明地址与签名都对。

格式不用选 —— 地址路径里带 `/open-apis/bot/` 时会自动按飞书群机器人的格式发送。
如果地址挂在自己的反向代理后面、路径被改掉了，把 **消息格式** 手动选成「飞书群机器人」。

## 签名校验怎么算

服务按飞书官方算法生成 `timestamp` 与 `sign`：

```
stringToSign = timestamp + "\n" + secret
sign         = base64(HMAC-SHA256(key = stringToSign, data = ""))
```

密钥填错、或者机器人开了签名校验但这里留空，飞书会返回
`code=19021`（sign match fail）。设置页顶栏的状态胶囊会显示这个错误，直到下一次发送成功。

## 通用 JSON

地址认不出是飞书、或手动选了「通用 JSON」时，请求体如下：

```json
{
  "source": "autodocker",
  "event": "update_available",
  "title": "web 的镜像有更新",
  "text": "镜像 `nginx:latest` 的远端内容已变更（aaa111 → bbb222）。\n容器：web\n镜像：nginx:latest",
  "image": "nginx:latest",
  "container": "web",
  "kind": "new_version",
  "time": "2026-10-24T21:00:00+08:00"
}
```

- `event`：`update_available`（发现更新）、`update_done` / `update_failed` /
  `update_rollback` 等（更新结果）、`test`（测试消息）。
- `kind`：`new_version`（浮动 tag 内容变了）或 `new_tag`（仓库出现更高版本 tag）。
- `text` 保留 Markdown 内联标记，方便接收端自己渲染。

请求头只有 `Content-Type: application/json` 与 `User-Agent: AutoDocker`，
不额外带鉴权；需要鉴权时把令牌放进 URL 或由你自己的网关处理。

## 更新结果会再推一条

另外两个渠道是「原地修改已发出的卡片」，webhook 改不了已发出的消息，
所以更新完成后会**另发一条**结果消息（成功 / 失败 / 已回滚）。
这也意味着一条更新对应两条 webhook 消息，属于预期行为。

## 排查

| 现象 | 多半是 |
| --- | --- |
| 顶栏显示「Webhook：未连接（…）」 | 最近一次发送失败了，悬停或进设置页看完整报错 |
| 飞书返回 `code=19021` | 签名密钥不对，或开了签名校验但没填 |
| 飞书返回 `code=19024` / 关键词不匹配 | 安全设置选了自定义关键词，但正文里没有那个词 |
| 群里有消息，但正文像原始 JSON | 消息格式被选成了「通用 JSON」，飞书机器人不认这个结构 |
| 测试消息能收到，更新提醒收不到 | 检查扫描是否真的发现了更新（容器页/通知页），webhook 只在有通知时发送 |
