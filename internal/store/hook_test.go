package store

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 脚本配置是用户在页面上设的，扫描每一轮都会重写监控项 —— 不能被冲掉。
//
// 真实现场：hook 项的容器引用写法一旦变化（打完 tag 后 Docker 报成裸 digest），
// 上一行的观测值全空，如果 hook_spec 跟着走 INSERT 的默认值，
// 用户配好的脚本就无声消失了，而且页面看起来还是「已保存」的样子。
func TestUpsertImageKeepsHook(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const ref, cid = "kuku2api:local", "c1"
	hook := &model.HookSpec{
		Enabled:   true,
		Image:     "docker:cli",
		Cwd:       "/opt/1panel/docker/compose/kuku2api",
		CheckCmd:  "./update.sh --check",
		UpdateCmd: "./update.sh",
		Mounts:    []string{"/var/run/docker.sock"},
		Env:       map[string]string{"PATH_PREFIX": "/usr/local/bin"},
	}
	if _, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "kuku2api",
		Origin: model.OriginContainer, Hook: hook,
	}); err != nil {
		t.Fatal(err)
	}

	// 模拟一轮扫描：不带 Hook 字段重写同一行
	if _, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "kuku2api",
		Origin: model.OriginContainer, Status: model.StatusUpToDate,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hook == nil {
		t.Fatal("扫描后 hook 配置丢失")
	}
	if got.Hook.Cwd != hook.Cwd || got.Hook.CheckCmd != hook.CheckCmd ||
		got.Hook.UpdateCmd != hook.UpdateCmd || got.Hook.Image != hook.Image {
		t.Fatalf("hook 字段被改写: %#v", got.Hook)
	}
	if len(got.Hook.Mounts) != 1 || got.Hook.Mounts[0] != "/var/run/docker.sock" {
		t.Fatalf("mounts 未原样保存: %v", got.Hook.Mounts)
	}
	if got.Hook.Env["PATH_PREFIX"] != "/usr/local/bin" {
		t.Fatalf("env 未原样保存: %v", got.Hook.Env)
	}
}

// 清除脚本配置时必须连「忽略本版本」的放行标记一起抹掉。
//
// hook 项写进 remote_digest 的是 hook@<镜像短ID> 这种合成标记，
// 留着它，切回注册表比对后永远对不上真实摘要 —— 等于埋了一个永不复位的抑制位。
func TestSetImageHookClear(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const ref, cid = "workbuddy:local", "c2"
	id, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "workbuddy",
		Origin: model.OriginContainer, Status: model.StatusUpdatePending,
		Hook: &model.HookSpec{Enabled: true, Cwd: "/opt/x", CheckCmd: "./update.sh --check"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetImageIgnoredDigest(id, "hook@deadbeefcafe"); err != nil {
		t.Fatal(err)
	}

	if err := s.SetImageHook(id, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hook != nil {
		t.Fatalf("hook 应已清除，实际 %#v", got.Hook)
	}
	if got.IgnoredDigest != "" {
		t.Fatalf("清除 hook 后合成抑制标记应一起清空，实际 %q", got.IgnoredDigest)
	}
	if got.Status != model.StatusUnknown {
		t.Fatalf("清除 hook 后状态应回到未检测，实际 %q", got.Status)
	}
}
