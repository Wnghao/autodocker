package scanner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hookx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// fakeHook 记录被要求执行的命令，并按脚本约定返回退出码。
type fakeHook struct {
	exit   int
	output string
	err    error
	cmds   []string
}

func (f *fakeHook) Run(ctx context.Context, spec *model.HookSpec, cmd, name string) (*hookx.Result, error) {
	f.cmds = append(f.cmds, cmd)
	if f.err != nil {
		return nil, f.err
	}
	return &hookx.Result{Exit: f.exit, Output: f.output}, nil
}

func newHookScanner(t *testing.T, hk hookx.Executor) (*Scanner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, opt: Options{Hook: hk}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return sc, st
}

func seedHookImage(t *testing.T, st *store.Store) *model.Image {
	t.Helper()
	_, err := st.UpsertImage(&model.Image{
		Reference: "kuku2api:local", ContainerID: "c1", ContainerName: "kuku2api",
		Origin: model.OriginContainer, ImageID: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Status: model.StatusUnknown,
		Hook: &model.HookSpec{
			Enabled: true, Image: "docker:cli", Cwd: "/opt/1panel/docker/compose/kuku2api",
			CheckCmd: "./update.sh --check", UpdateCmd: "./update.sh",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := st.FindImage("kuku2api:local", "c1")
	if err != nil {
		t.Fatal(err)
	}
	return img
}

var hookContainer = dockerx.Container{
	ID: "c1", Name: "kuku2api", Image: "kuku2api:local",
	ImageID: "sha256:aaaaaaaaaaaa4444bbbbffffffff0000cccc111122223333444455556666", State: "running", Running: true,
}

// 脚本报退出码 10 才算「有可用更新」，并且必须走 check_cmd 而不是 update_cmd。
//
// 这一条钉住两件事：hook 项绝不能顺手去做注册表比对（那种镜像根本不在仓库上，
// 比了只会得到一个「未知」）；通知正文里要带上脚本输出，否则用户只看得到
// 「有更新」三个字，不知道上游到底动了什么。
func TestCheckContainerHookReportsUpdate(t *testing.T) {
	hk := &fakeHook{exit: model.HookExitUpdateAvail, output: "上游有新提交：abc123 → def456\n需要重建"}
	sc, st := newHookScanner(t, hk)
	seedHookImage(t, st)

	r := sc.checkContainer(context.Background(), hookContainer, false)
	if !r.HasUpdate || !r.Notified {
		t.Fatalf("退出码 10 应判定为有更新并推送通知，实际 %+v", r)
	}
	if len(hk.cmds) != 1 || hk.cmds[0] != "./update.sh --check" {
		t.Fatalf("应执行 check_cmd，实际执行了 %v", hk.cmds)
	}

	img, err := st.FindImage("kuku2api:local", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if img.Status != model.StatusUpdatePending {
		t.Fatalf("状态应为待更新，实际 %q", img.Status)
	}
	if img.RemoteDigest != "hook@"+shortImageID(hookContainer.ImageID) {
		t.Fatalf("远端摘要应为合成的 hook 标记，实际 %q", img.RemoteDigest)
	}

	notes, err := st.ListNotifications(true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("应生成一条通知，实际 %d 条", len(notes))
	}
	if !strings.Contains(notes[0].Body, "上游有新提交") {
		t.Fatalf("通知正文缺少脚本输出: %q", notes[0].Body)
	}
	if !strings.Contains(notes[0].Body, "./update.sh") {
		t.Fatalf("通知正文应说明执行的是更新脚本: %q", notes[0].Body)
	}
}

// 用户点过「忽略本版本」后，只要本机镜像 ID 没变就不该再提醒。
//
// 这里最容易被漏掉：hook 项的远端摘要是在检测函数里现场合成的，
// 如果只把它写进数据库、没同步到用于判断的那份内存对象，
// 抑制条件永远不成立，每一轮扫描都会再推一次。
func TestCheckContainerHookRespectsAck(t *testing.T) {
	sc, st := newHookScanner(t, &fakeHook{exit: model.HookExitUpdateAvail, output: "有新提交"})
	seedHookImage(t, st)

	if r := sc.checkContainer(context.Background(), hookContainer, false); !r.Notified {
		t.Fatal("首轮应推送通知")
	}
	fresh, err := st.FindImage("kuku2api:local", "c1")
	if err != nil {
		t.Fatal(err)
	}
	// 用户在卡片上点了「忽略本版本」，记下的正是这个合成标记
	if err := st.SetImageIgnoredDigest(fresh.ID, fresh.RemoteDigest); err != nil {
		t.Fatal(err)
	}
	// 先把那条提醒处理掉，排除「待处理通知去重」的干扰 ——
	// 这里要验的只有摘要抑制那一条判断。
	notes, err := st.ListNotifications(true, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if err := st.SetNotificationCardState(n.ID, model.CardDone); err != nil {
			t.Fatal(err)
		}
	}

	if r := sc.checkContainer(context.Background(), hookContainer, false); r.Notified {
		t.Error("已放行的这一版不应再次推送通知")
	}

	// 镜像被重建（ID 变了）→ 标记跟着变，那是真的新内容，照常提醒
	rebuilt := hookContainer
	rebuilt.ImageID = "sha256:bbbbbbbbbbbb4444bbbbffffffff0000cccc111122223333444455556666"
	if r := sc.checkContainer(context.Background(), rebuilt, false); !r.Notified {
		t.Error("本机镜像已变化时应重新推送通知")
	}
}

// 退出码 0 是「已经是最新」，不是「脚本跑挂了」：状态要落回已是最新，且不生成通知。
func TestCheckContainerHookUpToDate(t *testing.T) {
	hk := &fakeHook{exit: model.HookExitUpToDate, output: "已是最新（abc123）"}
	sc, st := newHookScanner(t, hk)
	seedHookImage(t, st)

	if r := sc.checkContainer(context.Background(), hookContainer, false); r.HasUpdate {
		t.Fatalf("退出码 0 不应判定为有更新: %+v", r)
	}
	img, err := st.FindImage("kuku2api:local", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if img.Status != model.StatusUpToDate {
		t.Fatalf("状态应为已是最新，实际 %q", img.Status)
	}
}

// 其余退出码一律按检测失败处理，并把输出尾部写进列表小字。
// 否则页面上只有一个「未知」，没人知道是脚本没权限、还是镜像名写错了。
func TestCheckContainerHookUnexpectedExit(t *testing.T) {
	sc, st := newHookScanner(t, &fakeHook{exit: 127, output: "./update.sh: line 3: docker: not found"})
	seedHookImage(t, st)

	if r := sc.checkContainer(context.Background(), hookContainer, false); !r.Skipped {
		t.Fatalf("异常退出码应记为跳过: %+v", r)
	}
	img, err := st.FindImage("kuku2api:local", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if img.Status != model.StatusUnknown {
		t.Fatalf("状态应为未知，实际 %q", img.Status)
	}
	if !strings.Contains(img.LastError, "退出码 127") || !strings.Contains(img.LastError, "docker: not found") {
		t.Fatalf("失败原因应包含退出码与输出尾部: %q", img.LastError)
	}
}

// 辅助容器起不来（镜像没拉、路径不存在）时不能被当成「已是最新」。
func TestCheckContainerHookRunErrorKeepsUnknown(t *testing.T) {
	sc, st := newHookScanner(t, &fakeHook{err: errors.New("创建 hook 辅助容器失败: 镜像不存在")})
	seedHookImage(t, st)

	if r := sc.checkContainer(context.Background(), hookContainer, false); !r.Skipped {
		t.Fatalf("执行失败应记为跳过: %+v", r)
	}
	img, _ := st.FindImage("kuku2api:local", "c1")
	if img.Status != model.StatusUnknown {
		t.Fatalf("执行失败时状态应为未知，实际 %q", img.Status)
	}
	if !strings.Contains(img.LastError, "镜像不存在") {
		t.Fatalf("应保留失败原因: %q", img.LastError)
	}
}

// 执行器没注入（比如没挂 docker.sock 的跑法）时必须跳过而不是崩，
// 也绝不能悄悄回退成注册表比对 —— 那种镜像一比就是永久「未知」。
func TestCheckContainerHookWithoutExecutor(t *testing.T) {
	sc, st := newHookScanner(t, nil)
	seedHookImage(t, st)

	if r := sc.checkContainer(context.Background(), hookContainer, false); !r.Skipped {
		t.Fatalf("没有执行器时应跳过检测: %+v", r)
	}
}
