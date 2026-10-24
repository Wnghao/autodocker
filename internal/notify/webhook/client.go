// Package webhook 通过「入站 webhook 地址」单向推送通知。
//
// 与飞书自建应用、Telegram bot 不同，群机器人 webhook 只能收消息、发不出按钮回调：
// 发出去之后既改不了，也收不到点击。所以这里只做推送，更新结果会另发一条消息，
// 需要「点一下直接更新」的场景仍应使用飞书自建应用或 Telegram。
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 消息格式。
const (
	// FormatAuto 按地址自动识别；认不出来就走通用 JSON。
	FormatAuto    = "auto"
	FormatFeishu  = "feishu"
	FormatGeneric = "generic"
)

// Config 一个 webhook 接收端。
type Config struct {
	URL    string
	Secret string // 飞书群机器人「签名校验」密钥；通用格式不使用
	Format string // auto / feishu / generic
}

// Client 向固定的 webhook 地址推送消息。
type Client struct {
	mu  sync.RWMutex
	cfg Config
	hc  *http.Client

	// 最近一次发送的失败原因。webhook 没有长连接可看，只能拿真实请求当证据；
	// 空串表示「地址可用，最近一次发送没有报错」。
	lastErr string
}

// New 构造客户端。
func New(cfg Config) *Client {
	return &Client{
		cfg: normalize(cfg),
		hc:  &http.Client{Timeout: 20 * time.Second},
	}
}

// ValidURL 地址是否可用作 webhook 接收端。设置保存前用它挡掉明显写错的值。
func ValidURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return u.Host != ""
}

// normalize 统一格式取值：空串与 "auto" 同义，其余小写。
func normalize(cfg Config) Config {
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Secret = strings.TrimSpace(cfg.Secret)
	cfg.Format = strings.ToLower(strings.TrimSpace(cfg.Format))
	if cfg.Format == "" {
		cfg.Format = FormatAuto
	}
	return cfg
}

// Configured 地址是否已填且可用。
func (c *Client) Configured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return ValidURL(c.cfg.URL)
}

// Config 返回当前配置，供设置页回填。
func (c *Client) Config() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// SetOptions 更新配置。
func (c *Client) SetOptions(cfg Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = normalize(cfg)
	// 换了接收端，旧地址的成败不再说明问题。
	c.lastErr = ""
}

// Host 投递记录里显示的落点。只取主机名 —— 地址路径里通常带着机器人密钥，
// 不该被抄进通知表里。
func (c *Client) Host() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Format 返回实际使用的消息格式。
func (c *Client) Format() string {
	c.mu.RLock()
	f := c.cfg.Format
	raw := c.cfg.URL
	c.mu.RUnlock()
	return resolveFormat(f, raw)
}

// LastError 最近一次发送的失败原因；空串表示没有失败记录。
func (c *Client) LastError() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// resolveFormat 先看显式配置，再按地址识别。
//
// 飞书群机器人的回调路径是 /open-apis/bot/v2/hook/<token>，特征足够明显；
// 按路径而不是按域名判断，是为了让挂在自家反代后面的地址也能认出来。
func resolveFormat(format, raw string) string {
	switch format {
	case FormatFeishu, FormatGeneric:
		return format
	}
	u, err := url.Parse(raw)
	if err != nil {
		return FormatGeneric
	}
	if strings.Contains(strings.ToLower(u.Path), "/open-apis/bot/") {
		return FormatFeishu
	}
	return FormatGeneric
}

// Send 推送一条消息。返回错误时，接收端没有确认收到。
func (c *Client) Send(ctx context.Context, m Message) error {
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	if !ValidURL(cfg.URL) {
		return errors.New("Webhook 地址未配置或格式不对")
	}

	format := resolveFormat(cfg.Format, cfg.URL)
	body, err := buildPayload(format, cfg.Secret, m, time.Now())
	if err != nil {
		return err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "AutoDocker")

	resp, err := c.hc.Do(req)
	if err != nil {
		c.record(err.Error())
		return fmt.Errorf("请求 webhook 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("webhook 返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
		c.record(err.Error())
		return err
	}
	// 飞书对签名错误、频率限制等仍然返回 200，真正的结果在 code 里。
	if format == FormatFeishu {
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(raw, &out) == nil && out.Code != 0 {
			err := fmt.Errorf("飞书群机器人返回错误: code=%d msg=%s", out.Code, out.Msg)
			c.record(err.Error())
			return err
		}
	}

	c.record("")
	return nil
}

// record 记下本次发送的失败原因，成功时清空。
func (c *Client) record(errText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = errText
}

// buildPayload 按格式渲染请求体。
func buildPayload(format, secret string, m Message, now time.Time) (map[string]any, error) {
	switch format {
	case FormatFeishu:
		payload := map[string]any{
			"msg_type": "post",
			"content": map[string]any{
				"post": map[string]any{
					"zh_cn": map[string]any{
						"title":   m.Title,
						"content": postParagraphs(m.Lines),
					},
				},
			},
		}
		if secret != "" {
			ts := strconv.FormatInt(now.Unix(), 10)
			payload["timestamp"] = ts
			payload["sign"] = sign(ts, secret)
		}
		return payload, nil

	case FormatGeneric:
		return map[string]any{
			"source":    "autodocker",
			"event":     m.Event,
			"title":     m.Title,
			"text":      strings.Join(m.Lines, "\n"),
			"image":     m.Image,
			"container": m.Container,
			"kind":      m.Kind,
			"time":      now.Format(time.RFC3339),
		}, nil
	}
	return nil, fmt.Errorf("未知的 webhook 格式: %s", format)
}

// postParagraphs 把纯文本行转成飞书 post 的段落结构。
func postParagraphs(lines []string) []any {
	out := make([]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		out = append(out, []any{
			map[string]any{"tag": "text", "text": plain(line)},
		})
	}
	return out
}

// sign 计算飞书群机器人签名。
//
// 官方算法：以 "timestamp\nsecret" 为密钥、对空内容做 HMAC-SHA256，再 base64。
// 注意密钥与消息内容的位置很容易写反 —— 写反后请求同样返回 200，
// 只是 code 会告诉你签名不匹配。
func sign(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
