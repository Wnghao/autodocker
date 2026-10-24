package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/haihaipypy/autodocker/internal/config"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// hookTestServer 只装 handler 用到的那几样：库、配置、访问凭据。
func hookTestServer(t *testing.T, password string) (*Server, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, err := st.UpsertImage(&model.Image{
		Reference: "kuku2api:local", ContainerID: "c1", ContainerName: "kuku2api",
		Origin: model.OriginContainer,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		cfg: &config.Config{HookImage: "docker:cli"},
		st:  st,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.setAuth("admin", password)
	return s, st, id
}

func putHook(t *testing.T, s *Server, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/images/1/hook", bytes.NewBufferString(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	s.handleSetImageHook(rec, req)
	return rec
}

// 没设访问密码（匿名可写）的服务也允许保存脚本配置：
// 是否暴露到不可信网络由部署环境自己把关，代码里不再拦。
func TestSetImageHookSavesWithoutAuth(t *testing.T) {
	s, st, id := hookTestServer(t, "")
	rec := putHook(t, s, id, `{"hook":{"enabled":true,"cwd":"/opt/app","check_cmd":"./update.sh --check","update_cmd":"./update.sh"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("未启用密码时应能保存，实际 %d: %s", rec.Code, rec.Body.String())
	}
	img, _ := st.GetImage(id)
	if img.Hook == nil || !img.Hook.Enabled || img.Hook.Cwd != "/opt/app" {
		t.Fatalf("配置未正确落库: %#v", img.Hook)
	}
}

// 启用密码后按内容放行：结构不合法的一律 400，并且错误要说清楚是哪一项。
func TestSetImageHookValidation(t *testing.T) {
	s, st, id := hookTestServer(t, "secret")

	rec := putHook(t, s, id, `{"hook":{"enabled":true,"cwd":"opt/app","check_cmd":"./update.sh --check","update_cmd":"./update.sh"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("相对路径 cwd 应被拒，实际 %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("cwd")) {
		t.Fatalf("错误信息应指出是哪一项: %s", rec.Body.String())
	}

	rec = putHook(t, s, id, `{"hook":{"enabled":true,"cwd":"/opt/app","check_cmd":"./update.sh --check","update_cmd":"./update.sh","network":"container:abc"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界的 network 应被拒，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec = putHook(t, s, id, `{"hook":{"enabled":true,"cwd":"/opt/app","check_cmd":"./update.sh --check","update_cmd":"./update.sh"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法配置应保存成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	img, _ := st.GetImage(id)
	if img.Hook == nil || img.Hook.Cwd != "/opt/app" || !img.Hook.Enabled {
		t.Fatalf("配置未正确落库: %#v", img.Hook)
	}
}

// hook 传 null 表示清除；未启用的草稿允许保存（先填一半再去试检查脚本）。
func TestSetImageHookDraftAndClear(t *testing.T) {
	s, st, id := hookTestServer(t, "secret")

	rec := putHook(t, s, id, `{"hook":{"enabled":false,"cwd":"/opt/app","check_cmd":"./update.sh --check"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("未启用的草稿应能保存，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec = putHook(t, s, id, `{"hook":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清除应成功，实际 %d", rec.Code)
	}
	img, _ := st.GetImage(id)
	if img.Hook != nil {
		t.Fatalf("清除后仍留有配置: %#v", img.Hook)
	}

	rec = putHook(t, s, 9999, `{"hook":null}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的监控项应 404，实际 %d", rec.Code)
	}
}

// 页面上「测试检查」拿到的就是退出码 + 原始输出，没配脚本时要说清楚。
func TestCheckImageHookNeedsSpec(t *testing.T) {
	s, _, id := hookTestServer(t, "secret")
	req := httptest.NewRequest(http.MethodPost, "/api/images/1/hook/check", nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	s.handleCheckImageHook(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未配置脚本时应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}
