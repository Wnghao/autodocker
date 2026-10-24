package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hookx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

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

const (
	hookRef     = "kuku2api:local"
	hookCID     = "c1"
	hookCName   = "kuku2api"
	rebuiltImID = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
)

// hookDocker 假装守护进程：容器列表里那个同名容器已经换到了新镜像 ID，
// 镜像摘要查询返回空（本地构建的镜像本来就没有 RepoDigests）。
func hookDocker(t *testing.T) *dockerx.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/containers/json") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"Id":      "c1",
				"Names":   []string{"/" + hookCName},
				"Image":   hookRef,
				"ImageID": rebuiltImID,
				"State":   "running",
			}})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	dk, err := dockerx.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dk.Close() })
	return dk
}

func newHookUpdater(t *testing.T, hk hookx.Executor) (*Updater, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.UpsertImage(&model.Image{
		Reference: hookRef, ContainerID: hookCID, ContainerName: hookCName,
		Origin: model.OriginContainer, ImageID: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Status: model.StatusUpdatePending, RemoteDigest: "hook@111111111111",
	}); err != nil {
		t.Fatal(err)
	}
	taskID, err := st.CreateTask(&model.Task{ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName})
	if err != nil {
		t.Fatal(err)
	}
	up := New(hookDocker(t), st, Options{SelfContainer: "autodocker", Hook: hk},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return up, st, taskID
}

func hookSpec() *model.HookSpec {
	return &model.HookSpec{
		Enabled: true, Image: "docker:cli", Cwd: "/opt/1panel/docker/compose/kuku2api",
		CheckCmd: "./update.sh --check", UpdateCmd: "./update.sh",
	}
}

// 退出码 0 才算成功，而且必须执行 update_cmd。
// 成功后要把检测标记换成新镜像 ID —— 不换的话「忽略本版本」会压住之后每一版。
func TestRunHookSuccess(t *testing.T) {
	hk := &fakeHook{exit: model.HookExitUpToDate, output: "重建完成\n容器已启动"}
	up, st, taskID := newHookUpdater(t, hk)

	if err := up.RunHook(context.Background(), HookRequest{
		TaskID: taskID, ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName, Spec: hookSpec(),
	}); err != nil {
		t.Fatalf("退出码 0 应判为成功: %v", err)
	}
	if len(hk.cmds) != 1 || hk.cmds[0] != "./update.sh" {
		t.Fatalf("应执行 update_cmd，实际 %v", hk.cmds)
	}

	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.TaskSuccess {
		t.Fatalf("任务状态应为成功，实际 %q（error=%q）", task.Status, task.Error)
	}
	if !strings.Contains(task.Log, "重建完成") {
		t.Fatalf("任务日志应保留脚本输出: %q", task.Log)
	}

	img, err := st.FindImage(hookRef, hookCID)
	if err != nil {
		t.Fatal(err)
	}
	if img.Status != model.StatusUpToDate {
		t.Fatalf("状态应回到已是最新，实际 %q", img.Status)
	}
	if want := "hook@" + shortImageID(rebuiltImID); img.RemoteDigest != want {
		t.Fatalf("检测标记应换成新镜像 ID（%s），实际 %q", want, img.RemoteDigest)
	}
}

// 退出码 1 的含义是「升级失败，脚本已自行回滚」：旧版本还在跑。
// 这必须落成回滚而不是失败，否则卡片会告诉用户「更新失败」，而服务其实没断。
func TestRunHookRolledBackByScript(t *testing.T) {
	hk := &fakeHook{exit: model.HookExitRolled, output: "健康检查未通过，已回滚到 abc123"}
	up, st, taskID := newHookUpdater(t, hk)

	err := up.RunHook(context.Background(), HookRequest{
		TaskID: taskID, ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName, Spec: hookSpec(),
	})
	if err == nil {
		t.Fatal("退出码 1 应返回错误")
	}

	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskRolledBack {
		t.Fatalf("应记为已回滚，实际 %q", task.Status)
	}
	// 回滚后本机还是原来那一版：检测标记不能被换成新镜像
	img, _ := st.FindImage(hookRef, hookCID)
	if img.RemoteDigest != "hook@111111111111" {
		t.Fatalf("失败路径不应改动观测值，实际 %q", img.RemoteDigest)
	}
}

// 其余退出码（2 = 升级与回滚双失败、3 = 前置检查失败）按失败上报，
// 并把输出尾部带进错误信息 —— 页面上那行小字是唯一的线索。
func TestRunHookOtherExitIsFailure(t *testing.T) {
	hk := &fakeHook{exit: 2, output: strings.Repeat("构建日志\n", 30) + "回滚也失败了：端口被占用"}
	up, st, taskID := newHookUpdater(t, hk)

	err := up.RunHook(context.Background(), HookRequest{
		TaskID: taskID, ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName, Spec: hookSpec(),
	})
	if err == nil {
		t.Fatal("退出码 2 应返回错误")
	}
	if !strings.Contains(err.Error(), "退出码 2") || !strings.Contains(err.Error(), "端口被占用") {
		t.Fatalf("错误信息应含退出码与输出尾部: %q", err)
	}
	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskFailed {
		t.Fatalf("应记为失败，实际 %q", task.Status)
	}
}

// 辅助容器起不来时也要落任务状态，不能把任务永远挂在「进行中」。
func TestRunHookExecutorError(t *testing.T) {
	hk := &fakeHook{err: errors.New("创建 hook 辅助容器失败: 镜像 docker:cli 不存在")}
	up, st, taskID := newHookUpdater(t, hk)

	if err := up.RunHook(context.Background(), HookRequest{
		TaskID: taskID, ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName, Spec: hookSpec(),
	}); err == nil {
		t.Fatal("执行器报错应返回错误")
	}
	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskFailed {
		t.Fatalf("应记为失败，实际 %q", task.Status)
	}
}

// 自我保护与空配置必须在起辅助容器之前就挡住。
func TestRunHookRefusesSelfAndMissingSpec(t *testing.T) {
	hk := &fakeHook{exit: 0}
	up, _, taskID := newHookUpdater(t, hk)

	err := up.RunHook(context.Background(), HookRequest{
		TaskID: taskID, ImageRef: "autodocker:latest", ContainerID: "c9",
		ContainerName: "autodocker", Spec: hookSpec(),
	})
	if err == nil || !strings.Contains(err.Error(), "自身") {
		t.Fatalf("应拒绝更新自身容器，实际 %v", err)
	}
	if len(hk.cmds) != 0 {
		t.Fatal("被拒绝的请求不应真的执行脚本")
	}

	up2, _, taskID2 := newHookUpdater(t, hk)
	if err := up2.RunHook(context.Background(), HookRequest{
		TaskID: taskID2, ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName,
		Spec: &model.HookSpec{Enabled: true, Cwd: "/srv/app", CheckCmd: "./x"},
	}); err == nil || !strings.Contains(err.Error(), "更新脚本") {
		t.Fatalf("没写 update_cmd 时应拒绝，实际 %v", err)
	}
}

// 执行器没注入却收到 hook 更新请求，属于配置事故，必须报错而不是静默成功。
func TestRunHookWithoutExecutor(t *testing.T) {
	up, _, taskID := newHookUpdater(t, nil)
	err := up.RunHook(context.Background(), HookRequest{
		TaskID: taskID, ImageRef: hookRef, ContainerID: hookCID, ContainerName: hookCName, Spec: hookSpec(),
	})
	if err == nil || !strings.Contains(err.Error(), "脚本执行器") {
		t.Fatalf("应明确报错，实际 %v", err)
	}
}
