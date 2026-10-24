package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/config"
	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// 「未使用镜像」这格的口径：按镜像 ID 去重、只要还有容器引用就不算。
// 数错了用户会照着去清理，结果是删了在用的镜像 —— 所以钉死在这里。
func TestCountUnusedImages(t *testing.T) {
	images := []dockerx.Image{
		{ID: "sha256:aaa", Tags: []string{"nginx:latest"}},         // 容器在用
		{ID: "sha256:bbb", Tags: []string{"redis:7", "redis:7.2"}}, // 没人用，两个 tag 算一个
		{ID: "sha256:ccc"}, // 悬空层，没人用，要算
		{ID: ""},           // 拿不到 ID 的读不懂，跳过
	}
	used := map[string]bool{"sha256:aaa": true}

	if got := countUnusedImages(images, used); got != 2 {
		t.Errorf("未使用镜像应为 2（redis + 悬空层），实际 %d", got)
	}

	// 同一 ID 重复出现（响应里有重复项时）不能数两遍
	dup := []dockerx.Image{{ID: "sha256:ddd"}, {ID: "sha256:ddd"}}
	if got := countUnusedImages(dup, nil); got != 1 {
		t.Errorf("同一镜像出现两次应只算一个，实际 %d", got)
	}

	// 全部在用 → 0
	if got := countUnusedImages(images[:1], used); got != 0 {
		t.Errorf("容器在用的镜像不该计入，实际 %d", got)
	}

	// 没有镜像 → 0
	if got := countUnusedImages(nil, used); got != 0 {
		t.Errorf("空列表应为 0，实际 %d", got)
	}
}

// 容器列表这一份 JSON 是前端唯一的数据来源：字段必须还是平铺的
// （container_name、status 这些直接在顶层），access 挂在同一行上。
// containerRow 用内嵌结构体，哪天有人改成具名字段，页面会整列变「—」，
// 而接口返回 200，看不出问题 —— 所以钉在响应形状上。
func TestListContainersResponseShape(t *testing.T) {
	const cid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `[{"Id":"`+cid+`","Names":["/openlist"],
		  "Image":"openlist/openlist:v4.2.8","ImageID":"sha256:1","State":"running",
		  "Ports":[{"IP":"0.0.0.0","PrivatePort":443,"PublicPort":5244,"Type":"tcp"}]}]`)
	}))
	defer daemon.Close()

	dk, err := dockerx.New(daemon.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.UpsertImage(&model.Image{
		Reference: "openlist/openlist:v4.2.8", ContainerID: cid,
		ContainerName: "openlist", Origin: model.OriginContainer,
		Status: model.StatusUpToDate,
	}); err != nil {
		t.Fatal(err)
	}

	s := New(Deps{
		Cfg: &config.Config{}, Store: st, Docker: dk,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	rec := httptest.NewRecorder()
	s.handleListContainers(rec, httptest.NewRequest(http.MethodGet, "/api/containers", nil))

	var body struct {
		Total int `json:"total"`
		Items []struct {
			ID            int64  `json:"id"`
			ContainerName string `json:"container_name"`
			Reference     string `json:"reference"`
			Status        string `json:"status"`
			Access        []struct {
				Port int  `json:"port"`
				TLS  bool `json:"tls"`
			} `json:"access"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期形状: %v（%s）", err, rec.Body.String())
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("应有 1 条容器记录，实际 total=%d items=%d：%s", body.Total, len(body.Items), rec.Body.String())
	}
	item := body.Items[0]
	if item.ContainerName != "openlist" || item.Reference != "openlist/openlist:v4.2.8" || item.Status != "up_to_date" {
		t.Errorf("监控项字段没有平铺到顶层: %+v", item)
	}
	if len(item.Access) != 1 || item.Access[0].Port != 5244 || !item.Access[0].TLS {
		t.Errorf("访问地址应为 [{5244 tls}]，实际 %+v", item.Access)
	}
}

// Docker 连不上时列表照常返回，只是没有链接：不能整个容器页打不开。
func TestListContainersWithoutDocker(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertImage(&model.Image{
		Reference: "redis:7", ContainerID: "bbbb", ContainerName: "redis",
		Origin: model.OriginContainer, Status: model.StatusUpToDate,
	}); err != nil {
		t.Fatal(err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusInternalServerError)
	}))
	defer dead.Close()
	dk, err := dockerx.New(dead.URL)
	if err != nil {
		t.Fatal(err)
	}

	s := New(Deps{
		Cfg: &config.Config{}, Store: st, Docker: dk,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rec := httptest.NewRecorder()
	s.handleListContainers(rec, httptest.NewRequest(http.MethodGet, "/api/containers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("读不到 Docker 应仍是 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ContainerName string        `json:"container_name"`
			Access        []accessPoint `json:"access"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ContainerName != "redis" || len(body.Items[0].Access) != 0 {
		t.Errorf("应只给出监控项、不带链接，实际 %+v", body.Items)
	}
}
