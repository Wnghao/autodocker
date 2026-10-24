package hookx

import (
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 辅助容器的挂载表决定了脚本能不能真的碰到宿主机文件与守护进程。
// 少挂 docker.sock，脚本里的 docker 命令就是 command not found；
// 重复挂同一个目标，创建容器直接报 duplicate mount destination。
func TestBuildBinds(t *testing.T) {
	spec := &model.HookSpec{
		Cwd:    "/opt/1panel/docker/compose/kuku2api",
		Mounts: []string{"/opt/1panel/docker/compose/kuku2api", "/var/run/docker.sock", " ", "/data/certs"},
	}
	got := buildBinds(spec)

	joined := strings.Join(got, "\n")
	if n := strings.Count(joined, "/opt/1panel/docker/compose/kuku2api:"); n != 1 {
		t.Fatalf("项目目录应只挂一次，实际 %d 次: %v", n, got)
	}
	if n := strings.Count(joined, "/var/run/docker.sock:"); n != 1 {
		t.Fatalf("docker.sock 应只挂一次，实际 %d 次: %v", n, got)
	}
	if !strings.Contains(joined, "/data/certs:/data/certs") {
		t.Fatalf("额外挂载丢失: %v", got)
	}
	// 挂载一律按宿主机同路径，脚本里的绝对路径才对得上
	for _, b := range got {
		src, dst, ok := strings.Cut(b, ":")
		if !ok || src != dst {
			t.Fatalf("挂载应为 host:host 同路径: %q", b)
		}
	}

	// 只填 Cwd 也要自动补上 docker.sock —— 103 上那些 update.sh 全靠它。
	only := buildBinds(&model.HookSpec{Cwd: "/srv/app"})
	if len(only) != 2 || only[1] != "/var/run/docker.sock:/var/run/docker.sock" {
		t.Fatalf("应自动附加 docker.sock: %v", only)
	}
}

// 容器名只能含 [a-zA-Z0-9][a-. _]，且不能长到让 Docker 拒绝创建。
func TestContainerName(t *testing.T) {
	if got := containerName("kuku2 api/v3"); got != "autodocker-hook-kuku2-api-v3" {
		t.Fatalf("非法字符未净化: %q", got)
	}
	if got := containerName(""); got != "autodocker-hook-task" {
		t.Fatalf("空名应有兜底: %q", got)
	}
	long := containerName(strings.Repeat("x", 100))
	if len(long) != len("autodocker-hook-")+40 {
		t.Fatalf("名字未截断到安全长度: %d", len(long))
	}
}

// 输出尾部是页面上唯一的失败线索，超长时必须留末尾、而不是留开头。
func TestTailKeepsEnding(t *testing.T) {
	body := strings.Repeat("a", 100) + "FAILED: compose build exited"
	got := tail(body, 40)
	if !strings.HasSuffix(got, "compose build exited") {
		t.Fatalf("尾部丢失: %q", got)
	}
	if !strings.Contains(got, "前文省略") {
		t.Fatalf("应标明前文被省略: %q", got)
	}
	if tail("short", 40) != "short" {
		t.Fatal("未超长时不应改动内容")
	}
}
