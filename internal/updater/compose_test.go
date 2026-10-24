package updater

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hookx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

const (
	composeRef  = "openlist/openlist:v4.2.8"
	oldRef      = "openlist/openlist:v4.2.7"
	oldImgID    = "sha256:563fd6500e67c7fae1d3a71fdcd2f97e328a6a894e4a8c783542ed91cb23148b"
	composeCwd  = "/opt/1panel/apps/openlist/openlist"
	composeFile = composeCwd + "/docker-compose.yml"
)

// composeLabels 取自 103 上 openlist 容器的真实标签（compose 2.38.2 写的）。
func composeLabels() map[string]any {
	return map[string]any{
		"com.docker.compose.project":              "openlist",
		"com.docker.compose.service":              "openlist",
		"com.docker.compose.container-number":     "1",
		"com.docker.compose.oneoff":               "False",
		"com.docker.compose.version":              "2.38.2",
		"com.docker.compose.config-hash":          "34691e80",
		"com.docker.compose.project.working_dir":  composeCwd,
		"com.docker.compose.project.config_files": composeFile,
		"org.opencontainers.image.title":          "OpenList",
	}
}

// composePayload 是一份 inspect 响应，形态对齐真实守护进程返回的 JSON。
func composePayload(running bool) map[string]any {
	state := map[string]any{"Running": false, "Status": "exited", "ExitCode": 0}
	if running {
		state = map[string]any{"Running": true, "Status": "running", "Health": map[string]any{"Status": "healthy"}}
	}
	return map[string]any{
		"Id":    "c1",
		"Name":  "/openlist",
		"Image": oldImgID,
		"State": state,
		"Config": map[string]any{
			"Image":      "openlist/openlist:v4.2.7",
			"Hostname":   "c1short",
			"Env":        []any{"TZ=Asia/Shanghai"},
			"WorkingDir": "/opt/openlist",
			"Labels":     composeLabels(),
		},
		"HostConfig": map[string]any{
			"Binds":         []any{composeCwd + "/data:/opt/openlist/data"},
			"NetworkMode":   "openlist_default",
			"AutoRemove":    false,
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		},
		"NetworkSettings": map[string]any{
			"Networks": map[string]any{
				"openlist_default": map[string]any{"NetworkID": "nid", "IPAddress": "172.20.0.2"},
			},
		},
	}
}

func composeInspect() *dockerx.Inspect {
	insp := sampleInspect()
	insp.Name = "openlist"
	insp.Image = oldImgID
	insp.Config["Labels"] = composeLabels()
	return insp
}

func TestDetectCompose(t *testing.T) {
	ct := detectCompose(composeInspect())
	if ct == nil {
		t.Fatal("compose 容器应被识别出来")
	}
	if ct.project != "openlist" || ct.service != "openlist" {
		t.Fatalf("坐标不对: %+v", ct)
	}
	if ct.workDir != composeCwd || len(ct.files) != 1 || ct.files[0] != composeFile {
		t.Fatalf("项目路径不对: %+v", ct)
	}

	// 坐标缺一都不能交给 compose：宁可退回原路径，也不能重建出一个项目管不到的容器
	cases := map[string]func(*dockerx.Inspect){
		"非 compose 容器": func(i *dockerx.Inspect) { i.Config["Labels"] = map[string]any{"foo": "bar"} },
		"一次性容器": func(i *dockerx.Inspect) {
			i.Config["Labels"].(map[string]any)["com.docker.compose.oneoff"] = "True"
		},
		"没有服务名": func(i *dockerx.Inspect) {
			delete(i.Config["Labels"].(map[string]any), "com.docker.compose.service")
		},
		"没有配置文件": func(i *dockerx.Inspect) {
			i.Config["Labels"].(map[string]any)["com.docker.compose.project.config_files"] = ""
		},
		"工作目录是相对路径": func(i *dockerx.Inspect) {
			i.Config["Labels"].(map[string]any)["com.docker.compose.project.working_dir"] = "apps/openlist"
		},
	}
	for name, mutate := range cases {
		insp := composeInspect()
		mutate(insp)
		if got := detectCompose(insp); got != nil {
			t.Errorf("%s 不应被判定为可 compose 重建，得到 %+v", name, got)
		}
	}

	// compose 会把本次用过的每个 -f 都记进标签，所以第二次更新时，标签里会多出
	// 上一轮那份只存在于辅助容器里的临时覆盖（在 103 上用 compose 2.27 实测过）。
	// 认不出它，第二轮就会拿一个宿主机上不存在的文件去做预检 —— 从此再也走不了 compose。
	insp := composeInspect()
	insp.Config["Labels"].(map[string]any)["com.docker.compose.project.config_files"] =
		composeFile + "," + composeOverrideFile
	second := detectCompose(insp)
	if second == nil {
		t.Fatal("带着临时覆盖标签的容器仍应是 compose 容器")
	}
	if len(second.files) != 1 || second.files[0] != composeFile {
		t.Fatalf("临时覆盖文件不该被当成项目配置: %+v", second.files)
	}
	if got := second.extraMounts(); len(got) != 0 {
		t.Fatalf("不该为此把宿主机的 /tmp 挂进辅助容器，得到 %v", got)
	}
}

// 挂载清单不能多：项目目录已经由 spec.Cwd 挂了一遍，重复挂载辅助容器起不来。
// 目录之外的配置文件要单独挂，而且挂它所在的目录 —— 挂一个不存在的路径时
// Docker 会在宿主机上按目录补出来，等于在人家机器上造垃圾。
func TestComposeExtraMounts(t *testing.T) {
	if got := detectCompose(composeInspect()).extraMounts(); len(got) != 0 {
		t.Fatalf("配置文件在项目目录内时不该有额外挂载，得到 %v", got)
	}

	insp := composeInspect()
	insp.Config["Labels"].(map[string]any)["com.docker.compose.project.config_files"] =
		composeFile + ",/srv/extra/override.yml,/srv/extra/other.yml"
	if got := detectCompose(insp).extraMounts(); len(got) != 1 || got[0] != "/srv/extra" {
		t.Fatalf("应只补一个 /srv/extra（在项目目录之外且去重），得到 %v", got)
	}
}

func TestComposeScript(t *testing.T) {
	script := detectCompose(composeInspect()).composeScript(composeRef)

	for _, want := range []string{
		"--project-directory '" + composeCwd + "'",
		"-p 'openlist'",
		"-f '" + composeFile + "'",
		"config --quiet",
		"exit " + strconv.Itoa(composeFallbackExit),
		`image: "` + composeRef + `"`,
		"up -d --no-color 'openlist'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺少 %q:\n%s", want, script)
		}
	}
	// 预检排在重建之前，这样「退出码 20」才能被安全地当成「什么都没动」
	if strings.Index(script, "config --quiet") > strings.Index(script, "up -d") {
		t.Fatalf("compose 预检应当排在重建之前:\n%s", script)
	}
	// 项目自带的 compose 文件只能被读：唯一的写入必须是 /tmp 里那份镜像覆盖，
	// 用户的配置一个字都不能动。
	if strings.Contains(script, "sed -i") {
		t.Fatalf("脚本不该原地改写项目文件:\n%s", script)
	}
	if n := strings.Count(script, "cat >"); n != 1 || !strings.Contains(script, "cat > /tmp/ad-image.yml") {
		t.Fatalf("唯一的写入应是 /tmp 里的镜像覆盖文件（当前 %d 处写入）:\n%s", n, script)
	}
}

// 生成的是一段 shell，写错语法要等到辅助容器里才暴露，任务日志只剩一行退出码 2。
// 本机有 sh 就顺手做一次语法检查（没有则跳过，不影响别人在 Windows 上跑测试）。
func TestComposeScriptSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("本机没有 sh，跳过脚本语法检查")
	}
	script := detectCompose(composeInspect()).composeScript(composeRef)
	file := filepath.Join(t.TempDir(), "generated.sh")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(sh, "-n", file).CombinedOutput()
	if err != nil {
		t.Fatalf("生成的脚本语法不过（%v）：%s\n%s", err, out, script)
	}
}

// 「应用」脚本只做两件事：预检配置、对整项目 up -d，不碰项目文件。
func TestComposeApplyScript(t *testing.T) {
	script := detectCompose(composeInspect()).composeApplyScript()

	for _, want := range []string{
		"--project-directory '" + composeCwd + "'",
		"-p 'openlist'",
		"-f '" + composeFile + "'",
		"config --quiet",
		"exit " + strconv.Itoa(composeFallbackExit),
		"exit 21",
		"up -d --no-color\n",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺少 %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "config --quiet") > strings.Index(script, "up -d") {
		t.Fatalf("compose 预检应当排在 up 之前:\n%s", script)
	}
	if strings.Contains(script, "cat >") || strings.Contains(script, "sed -i") {
		t.Fatalf("应用脚本不该写任何文件:\n%s", script)
	}
	// 应用是对整个项目，不能带单个服务名，否则其余服务的配置改动不会生效
	if strings.Contains(script, "up -d --no-color '") {
		t.Fatalf("up 不应限定服务:\n%s", script)
	}

	if sh, err := exec.LookPath("sh"); err == nil {
		file := filepath.Join(t.TempDir(), "apply.sh")
		if werr := os.WriteFile(file, []byte(script), 0o600); werr != nil {
			t.Fatal(werr)
		}
		if out, serr := exec.Command(sh, "-n", file).CombinedOutput(); serr != nil {
			t.Fatalf("生成的脚本语法不过（%v）：%s\n%s", serr, out, script)
		}
	}
}

// 路径里带单引号（1Panel 不会，用户自己建的目录会）也不能把命令拆断。
func TestComposeScriptQuotes(t *testing.T) {
	insp := composeInspect()
	insp.Config["Labels"].(map[string]any)["com.docker.compose.project.working_dir"] = "/opt/my'app"
	insp.Config["Labels"].(map[string]any)["com.docker.compose.project.config_files"] = "/opt/my'app/c.yml"
	script := detectCompose(insp).composeScript("x:1")
	if !strings.Contains(script, `'`+"/opt/my"+`'\''`+`app/c.yml'`) {
		t.Fatalf("单引号未被安全转义:\n%s", script)
	}
}

// ---- Run 的分流 ----

type planHook struct {
	mu      sync.Mutex
	calls   []hookCall
	results []hookx.Result // 按调用次序取，用完则沿用最后一条
	err     error
}

type hookCall struct {
	spec *model.HookSpec
	cmd  string
}

func (p *planHook) Run(ctx context.Context, spec *model.HookSpec, cmd, name string) (*hookx.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, hookCall{spec: spec, cmd: cmd})
	if p.err != nil {
		return nil, p.err
	}
	i := len(p.calls) - 1
	if i >= len(p.results) {
		i = len(p.results) - 1
	}
	res := p.results[i]
	return &res, nil
}

func (p *planHook) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// daemon 假装 Docker 守护进程，并把收到的写操作记下来，
// 供「compose 路径绝不能自己动手造容器」这类断言使用。
type daemon struct {
	mu       sync.Mutex
	jobs     []string
	inspects map[string]map[string]any // 容器 ID -> inspect 响应，缺省给「运行中且健康」
	names    map[string]string         // 容器名 -> 容器 ID
}

func (d *daemon) note(method, path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobs = append(d.jobs, method+" "+path)
}

func (d *daemon) did(sub string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if strings.Contains(j, sub) {
			return true
		}
	}
	return false
}

func newDaemon(t *testing.T, d *daemon) *dockerx.Client {
	t.Helper()
	if d.names == nil {
		d.names = map[string]string{"openlist": "c1"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		d.note(r.Method, path)
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/containers/json"):
			var items []map[string]any
			for name, id := range d.names {
				items = append(items, map[string]any{
					"Id": id, "Names": []string{name}, "Image": composeRef, "State": "running",
				})
			}
			write(items)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/json") && strings.HasPrefix(path, "/containers/"):
			if p, ok := d.inspects[strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")]; ok {
				write(p)
				return
			}
			write(map[string]any{
				"Id": path, "Name": "/openlist", "Image": oldImgID,
				"State":  map[string]any{"Running": true, "Status": "running", "Health": map[string]any{"Status": "healthy"}},
				"Config": map[string]any{"Image": composeRef, "Labels": composeLabels()},
				"HostConfig": map[string]any{
					"Binds": []any{composeCwd + "/data:/opt/openlist/data"}, "NetworkMode": "openlist_default",
				},
			})
		case r.Method == http.MethodPost && strings.Contains(path, "/images/create"):
			write(map[string]any{"status": "Downloaded newer image"})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/tag"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(path, "/images/"):
			write(map[string]any{"RepoDigests": []string{"openlist/openlist@sha256:dddd"}})
		case r.Method == http.MethodPost && path == "/containers/create":
			write(map[string]any{"Id": "rebuilt1"})
		default:
			write(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	dk, err := dockerx.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dk.Close() })
	return dk
}

func newComposeUpdater(t *testing.T, hk *planHook, d *daemon, opt Options) (*Updater, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// 记录里写的是升级前的引用（发现更高版本 tag 的场景，与 103 上实测一致）：
	// 更新完成后必须跟着换成 composeRef，否则页面上一直挂着已删除的旧容器。
	if _, err := st.UpsertImage(&model.Image{
		Reference: oldRef, ContainerID: "c1", ContainerName: "openlist",
		Origin: model.OriginContainer, ImageID: oldImgID,
		Status: model.StatusUpdatePending, LocalDigest: "sha256:563fd",
	}); err != nil {
		t.Fatal(err)
	}
	taskID, err := st.CreateTask(&model.Task{ImageRef: composeRef, ContainerID: "c1", ContainerName: "openlist"})
	if err != nil {
		t.Fatal(err)
	}
	opt.SelfContainer = "autodocker"
	opt.Hook = hk
	opt.HealthCheckWait = 5 * time.Second
	return New(newDaemon(t, d), st, opt, slog.New(slog.NewTextHandler(io.Discard, nil))), st, taskID
}

func composeRequest(taskID int64) Request {
	return Request{TaskID: taskID, ImageRef: composeRef, ContainerID: "c1", ContainerName: "openlist"}
}

// 最要紧的一条：compose 化容器必须由 compose 重建。
// 自己造出来的新容器身上没有 compose 标签，`compose ps` 看不到它，
// 下次用户 compose up 又起一个同名服务 —— 项目从此管不到这个容器。
func TestRunUsesComposeForComposeContainer(t *testing.T) {
	d := &daemon{
		inspects: map[string]map[string]any{"c1": composePayload(true)},
		names:    map[string]string{"openlist": "c2"},
	}
	hk := &planHook{results: []hookx.Result{{Exit: 0, Output: " Container openlist Recreated\n Container openlist Started"}}}
	up, st, taskID := newComposeUpdater(t, hk, d, Options{})

	if err := up.Run(context.Background(), composeRequest(taskID)); err != nil {
		t.Fatalf("compose 更新应成功: %v", err)
	}
	if hk.count() != 1 {
		t.Fatalf("应只调用一次 compose，实际 %d 次", hk.count())
	}
	if !strings.Contains(hk.calls[0].cmd, "up -d") {
		t.Fatalf("辅助容器里应执行 compose up，得到 %q", hk.calls[0].cmd)
	}
	if hk.calls[0].spec.Cwd != composeCwd {
		t.Fatalf("辅助容器工作目录应是项目目录，得到 %q", hk.calls[0].spec.Cwd)
	}
	for _, banned := range []string{"POST /containers/create", "/rename", "POST /containers/c1/stop", "DELETE /containers/c1"} {
		if d.did(banned) {
			t.Fatalf("compose 路径不该自己做 %s，请求记录 %v", banned, d.jobs)
		}
	}
	if !d.did("POST /images/create") {
		t.Fatalf("必须先把目标镜像拉到本机，compose 才有新镜像可用: %v", d.jobs)
	}
	if !d.did("POST /images/sha256:563fd6500e67c7fae1d3a71fdcd2f97e328a6a894e4a8c783542ed91cb23148b/tag") {
		t.Fatalf("动手前要把旧镜像钉住，否则拉完新镜像就没了回滚把手: %v", d.jobs)
	}

	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskSuccess {
		t.Fatalf("任务应成功，实际 %q（error=%q）", task.Status, task.Error)
	}
	if task.BackupName != "" {
		t.Fatalf("compose 路径不保留重命名备份，BackupName 应为空，得到 %q", task.BackupName)
	}
	if !strings.Contains(task.Log, "由 compose 重建") {
		t.Fatalf("日志要交代这次走的是 compose: %q", task.Log)
	}
	// 容器 ID 和引用都要跟着走：只改 ID 的话，记录仍写着旧 tag，
	// 下一次点「立即更新」就会打在已经不存在的旧容器上（103 上真实复现过，任务直接 404）
	img, err := st.FindImage(composeRef, "c2")
	if err != nil {
		t.Fatalf("记录应跟着新容器和新引用走: %v（请求记录 %v）", err, d.jobs)
	}
	if img.Status != model.StatusUpToDate || img.LocalDigest != "sha256:dddd" {
		t.Fatalf("镜像记录未刷新: %+v", img)
	}
}

// 预检失败（约定的退出码 20）意味着 compose 这条路走不通，而且脚本保证还没碰过容器，
// 这时必须回落到原来的「按原配置重建」，而不是把更新直接判死。
func TestRunFallsBackWhenComposePreflightFails(t *testing.T) {
	d := &daemon{inspects: map[string]map[string]any{"c1": composePayload(true)}}
	hk := &planHook{results: []hookx.Result{{Exit: composeFallbackExit, Output: "辅助镜像里没有 docker compose"}}}
	up, st, taskID := newComposeUpdater(t, hk, d, Options{})

	if err := up.Run(context.Background(), composeRequest(taskID)); err != nil {
		t.Fatalf("回落到重建之后应成功: %v", err)
	}
	if !d.did("POST /containers/create") {
		t.Fatalf("应回落到逐容器重建，请求记录 %v", d.jobs)
	}
	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskSuccess {
		t.Fatalf("任务应成功，实际 %q", task.Status)
	}
	if !strings.Contains(task.Log, "改用按原配置重建") {
		t.Fatalf("日志要说明为什么没走 compose: %q", task.Log)
	}
}

// compose up 失败 + 开了自动回滚：拿同一份 compose 配置把镜像换回旧版本。
// 状态记成「已回滚」而不是「失败」—— 服务还在跑，只是没升上去。
func TestRunComposeFailureRollsBack(t *testing.T) {
	d := &daemon{
		inspects: map[string]map[string]any{"c1": composePayload(true)},
		names:    map[string]string{"openlist": "c2"},
	}
	hk := &planHook{results: []hookx.Result{
		{Exit: 1, Output: "Error response from daemon: Bind for 0.0.0.0:5244 failed: port is already allocated"},
		{Exit: 0, Output: " Container openlist Started"},
	}}
	up, st, taskID := newComposeUpdater(t, hk, d, Options{RollbackOnFailure: true})

	err := up.Run(context.Background(), composeRequest(taskID))
	if err == nil {
		t.Fatal("compose 失败应返回错误")
	}
	if hk.count() != 2 {
		t.Fatalf("回滚应再跑一次 compose，实际调用 %d 次", hk.count())
	}
	// 回滚用的是动手前先钉住的那个 tag：拉完新镜像之后，旧那份连名字都没有了，
	// 只有提前钉住的引用才能让 compose 认回上一版。
	if !strings.Contains(hk.calls[1].cmd, `image: "autodocker-rollback:`) {
		t.Fatalf("回滚要把旧镜像的钉住引用交给 compose，得到 %q", hk.calls[1].cmd)
	}
	if strings.Contains(hk.calls[1].cmd, composeRef) {
		t.Fatalf("回滚不该还指向新版: %q", hk.calls[1].cmd)
	}
	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskRolledBack {
		t.Fatalf("应记为已回滚，实际 %q", task.Status)
	}
	if !strings.Contains(task.Error, "退出码 1") {
		t.Fatalf("错误信息要带上 compose 的退出码: %q", task.Error)
	}
}

// 原本停着的 compose 服务不能因为「更新」被顺手启动：compose 的 up 一定会带起来，
// 所以起完还得按原状态停回去。
func TestRunComposeKeepsStoppedServiceStopped(t *testing.T) {
	d := &daemon{
		inspects: map[string]map[string]any{"c1": composePayload(false)},
		names:    map[string]string{"openlist": "c2"},
	}
	hk := &planHook{results: []hookx.Result{{Exit: 0, Output: " Container openlist Started"}}}
	up, st, taskID := newComposeUpdater(t, hk, d, Options{})

	if err := up.Run(context.Background(), composeRequest(taskID)); err != nil {
		t.Fatalf("更新应成功: %v", err)
	}
	if !d.did("POST /containers/c2/stop") {
		t.Fatalf("重建后应把该服务停回原状态，请求记录 %v", d.jobs)
	}
	task, _ := st.GetTask(taskID)
	if task.Status != model.TaskSuccess {
		t.Fatalf("任务应成功，实际 %q", task.Status)
	}
}

// 没配脚本执行器（拿不到 docker.sock 或没设 HOOK_IMAGE）时，compose 容器照旧能更新，
// 但日志必须说清这次没走 compose —— 不然用户只看到「成功」，不知道项目已经管不到它了。
func TestRunComposeWithoutExecutor(t *testing.T) {
	d := &daemon{inspects: map[string]map[string]any{"c1": composePayload(true)}}
	hk := &planHook{results: []hookx.Result{{Exit: 0}}}
	up, st, taskID := newComposeUpdater(t, hk, d, Options{})
	up.opt.Hook = nil

	if err := up.Run(context.Background(), composeRequest(taskID)); err != nil {
		t.Fatalf("应回落到重建并成功: %v", err)
	}
	if hk.count() != 0 {
		t.Fatal("没有执行器却仍然起了辅助容器")
	}
	if !d.did("POST /containers/create") {
		t.Fatalf("应走重建，请求记录 %v", d.jobs)
	}
	task, _ := st.GetTask(taskID)
	if !strings.Contains(task.Log, "未配置脚本执行器") {
		t.Fatalf("日志要交代原因，实际 %q", task.Log)
	}
}
