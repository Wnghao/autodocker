package dockerx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Port 的字段没有写 json 标签，全靠 Go 大小写不敏感地认 Docker 的键名。
// 这条测试钉住这件事：端口拿不到，容器名后面的跳转链接就整列消失，
// 而页面上一切照旧，不会有人发现。
func TestListContainersParsesPorts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 形态照抄真实守护进程（含 host 网络那种没有 PublicPort 的项）
		_, _ = w.Write([]byte(`[
		  {"Id":"aaaa","Names":["/openlist"],"Image":"openlist/openlist:v4.2.8","ImageID":"sha256:1",
		   "State":"running","Status":"Up 2 hours",
		   "Ports":[{"IP":"0.0.0.0","PrivatePort":443,"PublicPort":5244,"Type":"tcp"},
		            {"IP":"0.0.0.0","PrivatePort":80,"Type":"tcp"}]},
		  {"Id":"bbbb","Names":["/redis"],"Image":"redis:7","ImageID":"sha256:2",
		   "State":"exited","Status":"Exited (0)",
		   "Ports":[{"IP":"0.0.0.0","PrivatePort":6379,"PublicPort":6379,"Type":"tcp"}]}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("应列出 2 个容器，实际 %d", len(list))
	}

	ol := list[0]
	if !ol.Running || ol.Name != "openlist" {
		t.Fatalf("容器基础字段不对: %+v", ol)
	}
	if len(ol.Ports) != 2 {
		t.Fatalf("端口应解析出 2 条，实际 %+v", ol.Ports)
	}
	if p := ol.Ports[0]; p.IP != "0.0.0.0" || p.PrivatePort != 443 || p.PublicPort != 5244 || p.Type != "tcp" {
		t.Errorf("443 映射解析错了: %+v", p)
	}
	if p := ol.Ports[1]; p.PublicPort != 0 {
		t.Errorf("只 EXPOSE 的端口 PublicPort 应为 0，实际 %+v", p)
	}
	if len(list[1].Ports) != 1 {
		t.Errorf("redis 端口没解析出来: %+v", list[1].Ports)
	}
}
