package webhook

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/haihaipypy/autodocker/internal/model"
)

// Message 一条待推送的消息。
//
// Title / Lines 是各格式共用的正文；Event / Image / Container / Kind
// 只在通用 JSON 里出现，方便接收端做过滤和二次分发。
type Message struct {
	Title     string
	Lines     []string
	Event     string
	Image     string
	Container string
	Kind      string
}

// 通知正文由 scanner 用 Markdown 片段拼成（`tag`、**版本**）。
// 飞书 post 是纯文本，原样发出去会带着反引号和星号；这里只摘掉这两种标记。
var (
	mdCode = regexp.MustCompile("`([^`]+)`")
	mdBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

func plain(s string) string {
	// 脚本型监控项的正文里带 ``` 代码块围栏，飞书 post 不认这些标记。
	s = strings.ReplaceAll(s, "```", "")
	s = mdCode.ReplaceAllString(s, "$1")
	return mdBold.ReplaceAllString(s, "$1")
}

// splitLines 把可能多行的正文拆成独立段落。
func splitLines(body string) []string {
	out := make([]string, 0, 4)
	for _, line := range strings.Split(body, "\n") {
		out = append(out, line)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

func displayName(n *model.Notification) string {
	if n.ContainerName != "" {
		return n.ContainerName
	}
	return n.ImageRef
}

// BuildUpdateMessage 构造「发现更新」消息。
//
// 与飞书卡片不同，webhook 消息上没有按钮 —— 收到的人需要回面板操作。
func BuildUpdateMessage(n *model.Notification) Message {
	lines := splitLines(n.Body)
	lines = append(lines, "", "容器："+displayName(n), "镜像："+n.ImageRef)
	if n.Kind == model.NotifNewTag && n.LatestTag != "" {
		lines = append(lines, "当前版本："+orDash(n.LocalTag), "可用版本："+n.LatestTag)
	} else {
		lines = append(lines, "本地摘要："+orDash(n.LocalSummary), "远端摘要："+orDash(n.RemoteSummary))
	}
	if len(n.Aliases) > 0 {
		lines = append(lines, "同版本别名："+strings.Join(n.Aliases, "、"))
	}
	return Message{
		Title:     n.Title,
		Lines:     lines,
		Event:     "update_available",
		Image:     n.ImageRef,
		Container: n.ContainerName,
		Kind:      string(n.Kind),
	}
}

// BuildStateMessage 构造更新结果消息。
//
// 两个交互渠道是「原地改卡片」，webhook 改不了已发出的消息，
// 因此这里是一条新的消息，作为前一条提醒的后续。
func BuildStateMessage(n *model.Notification, state model.CardState, detail string) Message {
	title := "已处理"
	switch state {
	case model.CardUpdating:
		title = "正在更新容器"
	case model.CardDone:
		title = "更新完成"
	case model.CardFailed:
		title = "更新失败"
	case model.CardRollback:
		title = "更新失败，已回滚"
	case model.CardIgnored:
		title = "已忽略该版本"
	case model.CardSnoozed:
		title = "已设为稍后提醒"
	}
	lines := append(splitLines(detail), "", "容器："+displayName(n), "镜像："+n.ImageRef)
	return Message{
		Title:     title,
		Lines:     lines,
		Event:     "update_" + string(state),
		Image:     n.ImageRef,
		Container: n.ContainerName,
		Kind:      string(n.Kind),
	}
}

// BuildTestMessage 连通性测试消息。
func BuildTestMessage(cfg Config) Message {
	format := resolveFormat(normalize(cfg).Format, cfg.URL)
	lines := []string{
		"如果你看到了这条消息，说明这个 webhook 地址是通的。",
		"",
		fmt.Sprintf("消息格式：%s", formatName(format)),
	}
	if format == FormatFeishu && strings.TrimSpace(cfg.Secret) == "" {
		lines = append(lines, "签名校验：未启用（机器人安全设置里开了签名校验时，这里要填同一个密钥）")
	}
	lines = append(lines, "", "发现镜像更新时会往这里推送提醒；更新完成后还会再推一条结果。")
	return Message{
		Title: "AutoDocker 通知测试",
		Lines: lines,
		Event: "test",
	}
}

func formatName(format string) string {
	if format == FormatFeishu {
		return "飞书群机器人"
	}
	return "通用 JSON"
}
