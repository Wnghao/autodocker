package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

func TestResolveFormat(t *testing.T) {
	cases := []struct {
		name   string
		format string
		url    string
		want   string
	}{
		{"飞书地址自动识别", FormatAuto, "https://open.feishu.cn/open-apis/bot/v2/hook/xxx", FormatFeishu},
		{"反代地址按路径识别", FormatAuto, "http://nas.local/proxy/open-apis/bot/v2/hook/xxx", FormatFeishu},
		{"普通地址走通用格式", FormatAuto, "https://example.com/hooks/autodocker", FormatGeneric},
		{"显式飞书覆盖识别", FormatFeishu, "https://example.com/hooks/x", FormatFeishu},
		{"显式通用覆盖识别", FormatGeneric, "https://open.feishu.cn/open-apis/bot/v2/hook/x", FormatGeneric},
		{"空格式等同自动", "", "https://example.com/hook", FormatGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveFormat(tc.format, tc.url); got != tc.want {
				t.Fatalf("resolveFormat(%q, %q) = %q, want %q", tc.format, tc.url, got, tc.want)
			}
		})
	}
}

func TestSendFeishuPost(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("请求体不是 JSON: %v", err)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL + "/open-apis/bot/v2/hook/token"})
	if c.Format() != FormatFeishu {
		t.Fatalf("格式 = %q, want feishu", c.Format())
	}
	if err := c.Send(context.Background(), Message{
		Title: "web 有新版本可用",
		Lines: []string{"当前版本 `1.25`，可用 **1.27**。"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got["msg_type"] != "post" {
		t.Fatalf("msg_type = %v, want post", got["msg_type"])
	}
	if _, ok := got["sign"]; ok {
		t.Fatal("没配密钥时不该带签名")
	}

	content := got["content"].(map[string]any)["post"].(map[string]any)["zh_cn"].(map[string]any)
	if content["title"] != "web 有新版本可用" {
		t.Fatalf("title = %v", content["title"])
	}
	// 飞书 post 是纯文本，Markdown 标记要摘掉，否则群里会看到一堆星号和反引号。
	rows := content["content"].([]any)
	text := rows[0].([]any)[0].(map[string]any)["text"]
	if text != "当前版本 1.25，可用 1.27。" {
		t.Fatalf("正文 = %v", text)
	}
}

func TestSendFeishuSign(t *testing.T) {
	const secret = "test-secret"
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL + "/open-apis/bot/v2/hook/token", Secret: secret})
	if err := c.Send(context.Background(), Message{Title: "t", Lines: []string{"b"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ts, ok := got["timestamp"].(string)
	if !ok || ts == "" {
		t.Fatalf("缺少 timestamp: %v", got)
	}
	// 用固定算法重算一遍，防止签名把密钥和内容写反 —— 写反时请求仍是 200。
	want := sign(ts, secret)
	if got["sign"] != want {
		t.Fatalf("sign = %v, want %v", got["sign"], want)
	}
	if ts[0] < '0' || ts[0] > '9' {
		t.Fatalf("timestamp 不是 Unix 秒: %q", ts)
	}
}

func TestSendFeishuCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL + "/open-apis/bot/v2/hook/token", Secret: "bad"})
	err := c.Send(context.Background(), Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "19021") {
		t.Fatalf("err = %v, want 包含 19021", err)
	}
	if c.LastError() == "" {
		t.Fatal("失败原因应记下来供设置页显示")
	}
}

func TestSendGeneric(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := &model.Notification{
		ImageRef:      "nginx:latest",
		ContainerName: "web",
		Kind:          model.NotifNewVersion,
		Title:         "web 的镜像有更新",
		Body:          "镜像 `nginx:latest` 的远端内容已变更。",
		LocalSummary:  "aaa111",
		RemoteSummary: "bbb222",
	}

	c := New(Config{URL: srv.URL + "/hooks/autodocker"})
	if err := c.Send(context.Background(), BuildUpdateMessage(n)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got["source"] != "autodocker" || got["event"] != "update_available" {
		t.Fatalf("source/event = %v/%v", got["source"], got["event"])
	}
	if got["image"] != "nginx:latest" || got["container"] != "web" {
		t.Fatalf("image/container = %v/%v", got["image"], got["container"])
	}
	text, _ := got["text"].(string)
	if !strings.Contains(text, "nginx:latest") || !strings.Contains(text, "远端摘要：bbb222") {
		t.Fatalf("text = %q", text)
	}
	if c.LastError() != "" {
		t.Fatalf("成功发送后不应留下错误: %q", c.LastError())
	}
}

func TestSendHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.Send(context.Background(), Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want 包含 502", err)
	}
}

func TestUnconfigured(t *testing.T) {
	for _, raw := range []string{"", "open.feishu.cn/hook", "ftp://example.com/x"} {
		c := New(Config{URL: raw})
		if c.Configured() {
			t.Fatalf("%q 不该被判为已配置", raw)
		}
		if err := c.Send(context.Background(), Message{Title: "t"}); err == nil {
			t.Fatalf("%q 发送应报错", raw)
		}
	}
	if !New(Config{URL: "https://example.com/hook"}).Configured() {
		t.Fatal("合法地址应判为已配置")
	}
}
