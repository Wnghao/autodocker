package model

import (
	"strings"
	"testing"
)

// 脚本配置直接决定「在哪台机器上执行什么命令」，所以输入面必须收窄。
// 这里钉住几条最容易被误填、又最要命的约束。
func TestValidateHook(t *testing.T) {
	base := func() *HookSpec {
		return &HookSpec{
			Enabled:   true,
			Image:     "docker:cli",
			Cwd:       "/opt/1panel/docker/compose/app",
			CheckCmd:  "./update.sh --check",
			UpdateCmd: "./update.sh",
		}
	}

	if err := ValidateHook(base(), ""); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(h *HookSpec)
		defImage string
		want     string
	}{
		{"cwd 相对路径", func(h *HookSpec) { h.Cwd = "compose/app" }, "", "绝对路径"},
		{"cwd 为空", func(h *HookSpec) { h.Cwd = "" }, "", "绝对路径"},
		{"缺少检查命令", func(h *HookSpec) { h.CheckCmd = "" }, "", "check_cmd"},
		{"启用但无更新命令", func(h *HookSpec) { h.UpdateCmd = "" }, "", "update_cmd"},
		{"mount 相对路径", func(h *HookSpec) { h.Mounts = []string{"./data"} }, "", "绝对路径"},
		{"网络越界", func(h *HookSpec) { h.Network = "container:abc" }, "", "network"},
		{"没有可用镜像", func(h *HookSpec) { h.Image = "" }, "", "运行镜像"},
	}
	for _, tc := range cases {
		h := base()
		tc.mutate(h)
		err := ValidateHook(h, tc.defImage)
		if err == nil {
			t.Errorf("%s：应被拒绝", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s：错误信息应含 %q，实际 %q", tc.name, tc.want, err)
		}
	}

	// 未启用的草稿可以少配一点：没有镜像、没有更新命令都允许保存，
	// 否则用户没法先填一半再去试检查脚本。
	draft := base()
	draft.Enabled = false
	draft.Image = ""
	draft.UpdateCmd = ""
	if err := ValidateHook(draft, ""); err != nil {
		t.Errorf("未启用的草稿不应被拒: %v", err)
	}

	// 全局 HOOK_IMAGE 能顶替 spec.image —— 一批脚本型监控项共用同一个 runner 镜像。
	noImage := base()
	noImage.Image = ""
	if err := ValidateHook(noImage, "docker:cli"); err != nil {
		t.Errorf("应回落到全局默认镜像: %v", err)
	}
	if got := noImage.EffectiveImage("docker:cli"); got != "docker:cli" {
		t.Fatalf("EffectiveImage 应返回默认镜像，得到 %q", got)
	}
}
