// Package updater 执行容器更新：拉取新镜像并按原配置重建容器。
//
// 这是整个项目风险最高的环节。Docker 没有「原地更新容器」的能力，
// 只能 inspect 出原配置 → 停旧 → 建新 → 起新 → 健康检查。
// 因此这里的原则是：**配置零丢失 + 全程可回滚**。
//
// 实现上刻意把 Config / HostConfig 当作原始 JSON 原样搬运：
// 不做字段级的类型映射，就不会因为 Docker API 新增字段而悄悄丢配置。
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hookx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// Options 更新行为配置。
type Options struct {
	PullTimeout       time.Duration
	StopTimeoutSec    int
	HealthCheckWait   time.Duration
	RollbackOnFailure bool
	KeepOldContainer  bool
	SelfContainer     string
	SelfContainerID   string

	// Hook 是脚本型监控项的执行器。为 nil 时遇到 hook 监控项直接报错，
	// 而不是悄悄什么也不做 —— 「点了更新、任务显示成功、其实没跑脚本」是最坏的结果。
	Hook hookx.Executor
}

// Request 一次更新请求。
type Request struct {
	TaskID        int64
	ImageRef      string
	ContainerID   string
	ContainerName string
	TriggeredBy   string
}

// PullRequest 一次「只拉镜像」的请求。
type PullRequest struct {
	TaskID   int64
	ImageRef string
}

// Updater 更新执行器。同一时刻只允许一个更新在跑，避免并发重建互相踩踏。
type Updater struct {
	dk  *dockerx.Client
	st  *store.Store
	opt Options
	log *slog.Logger

	mu      sync.Mutex
	running bool
}

// New 构造更新执行器。
func New(dk *dockerx.Client, st *store.Store, opt Options, logger *slog.Logger) *Updater {
	opt = withDefaults(opt)
	return &Updater{dk: dk, st: st, opt: opt, log: logger}
}

func withDefaults(opt Options) Options {
	if opt.PullTimeout <= 0 {
		opt.PullTimeout = 30 * time.Minute
	}
	if opt.StopTimeoutSec <= 0 {
		opt.StopTimeoutSec = 30
	}
	if opt.SelfContainer == "" {
		opt.SelfContainer = "autodocker"
	}
	return opt
}

// SetOptions 热更新配置。
func (u *Updater) SetOptions(opt Options) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.opt = withDefaults(opt)
}

// Busy 是否有更新正在执行。
func (u *Updater) Busy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

// Run 执行一次容器更新，阻塞至结束。
// 返回非空 error 表示更新未成功（可能已回滚）。
func (u *Updater) Run(ctx context.Context, req Request) error {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		err := errors.New("已有更新任务正在执行，请稍后重试")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	u.running = true
	opt := u.opt
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		u.log.Info(line, "task_id", req.TaskID, "container", req.ContainerName)
		if err := u.st.AppendTaskLog(req.TaskID, "["+time.Now().Format("15:04:05")+"] "+line); err != nil {
			u.log.Warn("写入任务日志失败", "err", err)
		}
	}
	fail := func(err error) error {
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}

	// 自我保护：绝不能更新自己，否则进程被杀、任务中断
	if req.ContainerName == opt.SelfContainer ||
		(opt.SelfContainerID != "" && strings.HasPrefix(req.ContainerID, opt.SelfContainerID)) {
		logf("已阻止：目标 %s 是本服务自身", req.ContainerName)
		return fail(errors.New("拒绝更新自身容器"))
	}

	insp, err := u.dk.InspectContainer(ctx, req.ContainerID)
	if err != nil {
		return fail(err)
	}
	if len(insp.Config) == 0 || len(insp.HostConfig) == 0 {
		return fail(errors.New("容器配置不完整，无法安全重建"))
	}

	origName := insp.Name
	if origName == "" {
		origName = req.ContainerName
	}
	wasRunning := insp.State != nil && insp.State.Running
	stateText := ""
	if insp.State != nil {
		stateText = insp.State.Status
	}
	backupName := fmt.Sprintf("%s-old-%s", origName, time.Now().Format("20060102-150405"))

	// 自动删除的容器一旦停止就会被 Docker 立即回收，无法保留回滚现场
	if autoRemove, _ := insp.HostConfig["AutoRemove"].(bool); autoRemove {
		return fail(errors.New("目标容器启用了自动删除（--rm），停止后会被立即回收、无法回滚，已拒绝更新"))
	}

	// compose 归属判定：能被 compose 重建的容器就交回 compose。
	// 逐容器重建造出来的新容器身上没有 compose 标签，项目从此管不到它（详见 compose.go）。
	ct := detectCompose(insp)
	if ct != nil && opt.Hook == nil {
		logf("注意：该容器属于 compose 项目 %s，但未配置脚本执行器，本次按原配置重建", ct.project)
		ct = nil
	}

	// compose 这条路不留重命名的备份容器，任务里也就不写这个字段
	taskBackup := backupName
	if ct != nil {
		taskBackup = ""
	}
	if err := u.st.StartTask(req.TaskID, taskBackup); err != nil {
		u.log.Warn("更新任务状态失败", "err", err)
	}
	logf("开始更新容器 %s（镜像 %s，原状态 %s）", origName, req.ImageRef, stateText)
	if ct != nil {
		logf("该容器由 compose 管理（项目 %s / 服务 %s），改用 docker compose 重建", ct.project, ct.service)
	}

	// 先把重建所需配置准备好，任何问题都在动手之前暴露
	cfg, hostCfg, netCfg, warns := buildSpec(insp, req.ImageRef)
	for _, w := range warns {
		logf("注意：%s", w)
	}

	// 拉取新镜像
	logf("拉取镜像 %s", req.ImageRef)
	pullCtx, cancelPull := context.WithTimeout(ctx, opt.PullTimeout)
	err = u.dk.PullImage(pullCtx, req.ImageRef, func(format string, args ...any) {
		logf(format, args...)
	})
	cancelPull()
	if err != nil {
		return fail(err)
	}
	logf("镜像拉取完成")

	// compose 化容器在这里分叉：由 compose 起的服务必须还让 compose 起。
	if ct != nil {
		handled, cerr := u.composeUpgrade(ctx, req, insp, ct, wasRunning, opt, logf)
		if handled {
			return cerr
		}
		// 走到这里说明 compose 预检就没过，脚本保证还没碰过任何容器
		logf("该服务无法交给 compose，改用按原配置重建")
	}

	// 停旧容器，腾出容器名与端口
	if wasRunning {
		logf("停止旧容器")
		if err := u.dk.StopContainer(ctx, req.ContainerID, opt.StopTimeoutSec); err != nil {
			return fail(err)
		}
	}

	// 重命名旧容器：既腾出原名，也保留回滚现场
	if err := u.dk.RenameContainer(ctx, req.ContainerID, backupName); err != nil {
		if wasRunning {
			_ = u.dk.StartContainer(ctx, req.ContainerID)
		}
		return fail(err)
	}
	logf("旧容器已重命名为 %s", backupName)

	// 用原配置创建新容器
	newID, err := u.dk.CreateContainer(ctx, cfg, hostCfg, netCfg, origName)
	if err != nil {
		logf("创建新容器失败，开始回滚")
		u.rollback(ctx, "", req.ContainerID, origName, wasRunning, logf)
		return fail(err)
	}
	logf("新容器已创建 %s", shortID(newID))

	if err := u.dk.StartContainer(ctx, newID); err != nil {
		logf("启动新容器失败，开始回滚")
		u.rollback(ctx, newID, req.ContainerID, origName, wasRunning, logf)
		return fail(err)
	}

	// 健康检查：确认新容器真的活了再清理旧容器
	if wasRunning {
		ok, msg := u.waitHealthy(ctx, newID, opt.HealthCheckWait, logf)
		if !ok {
			if opt.RollbackOnFailure {
				logf("健康检查未通过（%s），执行回滚", msg)
				u.rollback(ctx, newID, req.ContainerID, origName, wasRunning, logf)
				_ = u.st.FinishTask(req.TaskID, model.TaskRolledBack, "", msg)
				return errors.New(msg)
			}
			logf("健康检查未通过（%s），但已关闭自动回滚，保留新容器", msg)
			_ = u.st.FinishTask(req.TaskID, model.TaskFailed, newID, msg)
			return errors.New(msg)
		}
		logf("健康检查通过：%s", msg)
	}

	// 更新成功
	if opt.KeepOldContainer {
		logf("保留旧容器 %s（按配置未删除）", backupName)
	} else {
		logf("清理旧容器 %s", backupName)
		if err := u.dk.RemoveContainer(ctx, req.ContainerID, false, false); err != nil {
			logf("警告：删除旧容器失败（%v），可手动清理 %s", err, backupName)
		}
	}
	logf("更新完成：%s 已运行在新镜像上", origName)

	// 刷新镜像记录的本地观测值：容器 ID 已变，摘要也应以新镜像为准
	u.refreshAfterUpgrade(ctx, req.ImageRef, req.ContainerID, newID, origName)

	_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, newID, "")
	return nil
}

// refreshAfterUpgrade 容器被重建之后，把库里那条监控记录对到新容器上。
//
// 不刷的话，页面会拿着「已不存在的容器 ID + 旧镜像摘要」继续判断，
// 下一次扫描要么查不到容器，要么一直显示「有更新」。
func (u *Updater) refreshAfterUpgrade(ctx context.Context, imageRef, oldContainerID, newID, name string) {
	img, ferr := u.st.FindImageByContainer(oldContainerID)
	if ferr != nil {
		return
	}
	localDigest := img.LocalDigest
	if newInsp, ierr := u.dk.InspectContainer(ctx, newID); ierr == nil {
		if _, d, derr := u.dk.DigestForImage(ctx, newInsp.Image, imageRef); derr == nil && d != "" {
			localDigest = d
		}
	}
	_ = u.st.UpdateImageLocal(img.ID, imageRef, newID, name, localDigest, tagOf(imageRef))
	_ = u.st.UpdateImageCheck(img.ID, model.StatusUpToDate, localDigest, "", nil, img.Mode, "")
}

// Pull 只把镜像拉到本地，不碰任何容器。
//
// 用于「本机有这个镜像、但没有任何容器在用它」的情形：这里要的只是把本地
// 那份换成新的，没有容器需要重启，也就没有回滚一说。
// 它与 Run 共用同一把互斥锁 —— 让拉取和重建错开，免得两边同时折腾同一批镜像层。
func (u *Updater) Pull(ctx context.Context, req PullRequest) error {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		err := errors.New("已有更新任务正在执行，请稍后重试")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	u.running = true
	opt := u.opt
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		u.log.Info(line, "task_id", req.TaskID, "image", req.ImageRef)
		if err := u.st.AppendTaskLog(req.TaskID, "["+time.Now().Format("15:04:05")+"] "+line); err != nil {
			u.log.Warn("写入任务日志失败", "err", err)
		}
	}

	if err := u.st.StartTask(req.TaskID, ""); err != nil {
		u.log.Warn("更新任务状态失败", "err", err)
	}
	logf("开始拉取镜像 %s", req.ImageRef)

	pullCtx, cancel := context.WithTimeout(ctx, opt.PullTimeout)
	err := u.dk.PullImage(pullCtx, req.ImageRef, func(format string, args ...any) {
		logf(format, args...)
	})
	cancel()
	if err != nil {
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	logf("镜像拉取完成")

	// 拉取之后本地那份已经换掉了：镜像 ID、大小、摘要都得重新采一遍，
	// 否则页面上还挂着旧摘要，状态会一直显示成「有更新」。
	if img, ferr := u.st.FindImage(req.ImageRef, ""); ferr == nil {
		fresh := *img
		if list, lerr := u.dk.ListImages(ctx); lerr == nil {
			for _, it := range list {
				matched := false
				for _, tag := range it.Tags {
					if tag == req.ImageRef {
						matched = true
						break
					}
				}
				if matched {
					fresh.ImageID = it.ID
					fresh.SizeBytes = it.Size
					fresh.ImageCreated = it.Created
					break
				}
			}
		}
		if _, d, derr := u.dk.DigestForImage(ctx, fresh.ImageID, req.ImageRef); derr == nil {
			fresh.LocalDigest = d
		}
		fresh.Status = model.StatusUpToDate
		fresh.LastError = ""
		fresh.LatestTag = ""
		fresh.Aliases = nil
		if _, uerr := u.st.UpsertImage(&fresh); uerr != nil {
			u.log.Warn("刷新镜像记录失败", "err", uerr)
		}
	}

	_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, "", "")
	return nil
}

// HookRequest 一次「用项目自带脚本更新」请求。
type HookRequest struct {
	TaskID        int64
	ImageRef      string
	ContainerID   string
	ContainerName string
	Spec          *model.HookSpec
}

// RunHook 执行监控项自带的更新脚本，阻塞至结束。
//
// 本地构建的镜像（compose build 出来的那种）没有注册表可比对，更新动作也轮不到
// Docker API 重建 —— 「怎么更新」写在项目目录里的 update.sh 里。这里只负责把它
// 跑一遍，再把退出码翻译成任务状态。
//
// 回滚一概不做：脚本自己就是带回滚的（退出码 1 的含义是「已回滚」），
// 这里再猜一次现场，只会把脚本已经处理好的东西搅乱。
func (u *Updater) RunHook(ctx context.Context, req HookRequest) error {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		err := errors.New("已有更新任务正在执行，请稍后重试")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	u.running = true
	opt := u.opt
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	name := req.ContainerName
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		u.log.Info(line, "task_id", req.TaskID, "container", name)
		if err := u.st.AppendTaskLog(req.TaskID, "["+time.Now().Format("15:04:05")+"] "+line); err != nil {
			u.log.Warn("写入任务日志失败", "err", err)
		}
	}
	fail := func(err error) error {
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}

	if req.ContainerName == opt.SelfContainer {
		logf("已阻止：目标 %s 是本服务自身", name)
		return fail(errors.New("拒绝更新自身容器"))
	}
	if opt.Hook == nil {
		return fail(errors.New("未配置脚本执行器，无法执行 hook 更新"))
	}
	spec := req.Spec
	if spec == nil || strings.TrimSpace(spec.UpdateCmd) == "" {
		return fail(errors.New("该监控项没有配置更新脚本"))
	}

	if err := u.st.StartTask(req.TaskID, ""); err != nil {
		u.log.Warn("更新任务状态失败", "err", err)
	}
	logf("开始以更新脚本更新 %s（工作目录 %s）", name, spec.Cwd)

	runCtx, cancelRun := context.WithTimeout(ctx, time.Duration(spec.UpdateTimeoutSec())*time.Second)
	res, err := opt.Hook.Run(runCtx, spec, spec.UpdateCmd, name)
	cancelRun()
	if err != nil {
		logf("更新脚本未能执行完成")
		return fail(err)
	}
	for _, line := range lastLines(res.Output, 20) {
		logf("%s", line)
	}

	switch res.Exit {
	case model.HookExitUpToDate: // 0：脚本报告更新完成
	case model.HookExitRolled: // 1：升级失败，脚本已自行回滚
		msg := "更新脚本执行失败，已由脚本回滚到原版本（详见任务日志）"
		logf("%s", msg)
		_ = u.st.FinishTask(req.TaskID, model.TaskRolledBack, req.ContainerID, msg)
		return errors.New(msg)
	default:
		msg := fmt.Sprintf("更新脚本退出码 %d（0=成功 1=已回滚）：%s", res.Exit, briefTail(res.Output, 400))
		logf("更新脚本异常退出")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, req.ContainerID, msg)
		return errors.New(msg)
	}
	logf("更新脚本执行完成")

	// 回写观测值：脚本重建过镜像，本机那份的镜像 ID 已经变了。
	// 检测标记（hook@<镜像短ID>）必须跟着换成新的，否则「忽略本版本」会压住
	// 这一版之后的每一次提醒 —— 那是抑制坏了，不是内容没变。
	newCID := req.ContainerID
	rebuilt := false
	if img, ferr := u.st.FindImage(req.ImageRef, req.ContainerID); ferr == nil {
		marker := img.RemoteDigest
		if c, ok := u.findContainerByName(ctx, name); ok {
			newCID = c.ID
			rebuilt = c.ImageID != img.ImageID
			marker = "hook@" + shortImageID(c.ImageID)
			localDigest := img.LocalDigest
			if _, d, derr := u.dk.DigestForImage(ctx, c.ImageID, c.Image); derr == nil && d != "" {
				localDigest = d
			}
			_ = u.st.UpdateImageLocal(img.ID, c.Image, c.ID, c.Name, localDigest, tagOf(c.Image))
		}
		_ = u.st.UpdateImageCheck(img.ID, model.StatusUpToDate, marker, "", nil, img.Mode, "")
	}

	if rebuilt {
		logf("更新完成：%s 已跑在新版本上", name)
	} else {
		// 脚本退出 0 但镜像 ID 没变 = 它自己判定无需改动（已是最新）。
		// 这种情况说「已跑在新版本上」是假的。
		logf("更新完成：%s 镜像没有变化（脚本判定无需更新）", name)
	}
	_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, newCID, "")
	return nil
}

// findContainerByName 按容器名找当前容器。
// 脚本型更新常由 compose 重建容器，ID 会变，只能靠名字回到监控项上。
func (u *Updater) findContainerByName(ctx context.Context, name string) (dockerx.Container, bool) {
	if name == "" {
		return dockerx.Container{}, false
	}
	list, err := u.dk.ListContainers(ctx)
	if err != nil {
		u.log.Warn("列举容器失败", "err", err)
		return dockerx.Container{}, false
	}
	for _, c := range list {
		if c.Name == name {
			return c, true
		}
	}
	return dockerx.Container{}, false
}

// lastLines 取输出末尾 n 行，供逐行写进任务日志。
func lastLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := make([]string, 0, n)
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// briefTail 把脚本输出压成一行短摘要，供页面的失败小字展示。
func briefTail(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// shortImageID 把 sha256:… 的镜像 ID 压成短码。
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// rollback 把现场恢复到更新之前：删掉新容器、旧容器改回原名并（必要时）重新启动。
func (u *Updater) rollback(ctx context.Context, newID, oldID, origName string, wasRunning bool, logf func(string, ...any)) {
	if newID != "" {
		logf("回滚：删除新容器 %s", shortID(newID))
		if err := u.dk.RemoveContainer(ctx, newID, true, false); err != nil {
			logf("回滚：删除新容器失败（%v）", err)
		}
	}
	logf("回滚：旧容器恢复原名 %s", origName)
	if err := u.dk.RenameContainer(ctx, oldID, origName); err != nil {
		logf("回滚：恢复容器名失败（%v），旧容器仍为备份名，请手动处理", err)
	}
	if wasRunning {
		logf("回滚：重新启动旧容器")
		if err := u.dk.StartContainer(ctx, oldID); err != nil {
			logf("回滚：重新启动旧容器失败（%v），请手动启动", err)
		}
	}
}

// waitHealthy 在新容器启动后做一段健康确认。
//
// 判断依据：
//   - 容器退出 → 立即失败；
//   - 定义了 HEALTHCHECK 且变为 healthy → 通过；变为 unhealthy → 失败；
//   - 未定义 HEALTHCHECK → 在观察窗口内持续运行即视为通过。
func (u *Updater) waitHealthy(ctx context.Context, id string, window time.Duration, logf func(string, ...any)) (bool, string) {
	if window <= 0 {
		window = 15 * time.Second
	}
	deadline := time.Now().Add(window)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		running, health, status, err := u.dk.State(ctx, id)
		if err != nil {
			return false, "读取新容器状态失败: " + err.Error()
		}
		switch {
		case !running:
			return false, "新容器已退出（状态 " + status + "）"
		case health == "healthy":
			return true, "健康检查状态 healthy"
		case health == "unhealthy":
			return false, "健康检查状态 unhealthy"
		}
		if time.Now().After(deadline) {
			if health == "starting" {
				return false, "健康检查在观察窗口内未完成（仍为 starting）"
			}
			return true, "容器保持运行（未定义健康检查或已通过观察）"
		}
		select {
		case <-ctx.Done():
			return false, "等待健康检查被取消"
		case <-ticker.C:
		}
	}
}

// buildSpec 从 inspect 结果还原出重建容器所需的全部配置。
//
// 关键动作有三个：
//  1. Config 与 HostConfig 整份克隆（原样透传，不丢字段），再逐条修正少数「不能照抄」的字段；
//  2. 补上匿名卷这处 inspect 才有、而创建时必须显式给出的信息；
//  3. 只把用户定义网络的接入设置搬回去，内置网络交给 NetworkMode 决定。
//
// targetRef 是本次要更新到的镜像引用。必须显式写入 Config.Image：
// inspect 里的 Config.Image 是「容器创建时用的引用」，在「更高版本 tag」场景下
// 它仍是旧 tag，照抄会重建出一个依旧跑着旧镜像的容器（表面成功、实际什么都没更新）。
func buildSpec(insp *dockerx.Inspect, targetRef string) (cfg, hostCfg, netCfg map[string]any, warns []string) {
	cfg = cloneMap(insp.Config)
	hostCfg = cloneMap(insp.HostConfig)

	if targetRef != "" {
		cfg["Image"] = targetRef
	}

	// Hostname 默认被 Docker 填成容器短 ID。照抄会把「旧容器的 ID」带进新容器，
	// 既没有意义，又会让依赖 hostname 判断的日志/探针产生误导。
	// 只在它确实等于旧容器 ID 前缀（即自动分配）时才清掉；用户手写的 --hostname 保持不动。
	if h, _ := cfg["Hostname"].(string); h != "" && strings.HasPrefix(insp.ID, h) {
		delete(cfg, "Hostname")
	}

	// 匿名卷补齐：容器启动时由 VOLUME 指令隐式创建的卷不会出现在 Binds 里，
	// 若不显式指回原卷名，重建后会挂到一个全新的空卷上，数据即丢失。
	binds := toStringSlice(hostCfg["Binds"])
	binds = appendAnonymousVolumes(binds, toAnySlice(hostCfg["Mounts"]), insp.Mounts)
	if len(binds) > 0 {
		hostCfg["Binds"] = binds
	}

	if v := toStringSlice(hostCfg["VolumesFrom"]); len(v) > 0 {
		warns = append(warns, fmt.Sprintf("该容器使用 --volumes-from %v，请确认被引用容器仍然存在", v))
	}
	if v := toStringSlice(hostCfg["Links"]); len(v) > 0 {
		warns = append(warns, fmt.Sprintf("该容器使用了旧式 --link %v，重建后别名可能失效", v))
	}
	if insp.State != nil && insp.State.Running {
		if mode, _ := hostCfg["NetworkMode"].(string); mode == "host" {
			warns = append(warns, "该容器使用 host 网络，重建期间端口会短暂不可用")
		}
	}

	netCfg = buildNetworkingConfig(insp)
	return cfg, hostCfg, netCfg, warns
}

// buildNetworkingConfig 构造创建容器时需要的 NetworkingConfig。
//
// 这里只搬「用户定义网络」上的接入设置，并且只搬接入真正用得上的字段。两处必须剔除：
//
//   - **内置网络**（bridge / host / none / nat）：容器接哪张网已经由
//     HostConfig.NetworkMode 决定了，再往 EndpointsConfig 里塞一份会被守护进程直接拒绝
//     （container cannot be connected to network endpoints: host）。
//     这也是「用了 host 网络的容器更新失败」最常见的成因。
//
//   - **NetworkID / EndpointID / Gateway / IPAddress / MacAddress**：这些是运行时填充的，
//     原样搬回去会被 Docker 当成「用户指定了 IP/MAC」，在旧容器还占着地址时报冲突。
func buildNetworkingConfig(insp *dockerx.Inspect) map[string]any {
	// 共享其它容器网络栈时，接哪张网由被共享的容器决定，这里一个端点都不能传
	mode, _ := insp.HostConfig["NetworkMode"].(string)
	if strings.HasPrefix(mode, "container:") {
		return nil
	}
	if insp.NetworkSettings == nil || len(insp.NetworkSettings.Networks) == 0 {
		return nil
	}

	eps := map[string]any{}
	for name, ep := range insp.NetworkSettings.Networks {
		if ep == nil || isBuiltinNetwork(name) {
			continue
		}
		entry := map[string]any{}
		if ep.IPAMConfig != nil &&
			(ep.IPAMConfig.IPv4Address != "" || ep.IPAMConfig.IPv6Address != "" || len(ep.IPAMConfig.LinkLocalIPs) > 0) {
			entry["IPAMConfig"] = ep.IPAMConfig
		}
		if len(ep.Links) > 0 {
			entry["Links"] = ep.Links
		}
		if len(ep.Aliases) > 0 {
			entry["Aliases"] = ep.Aliases
		}
		if len(ep.DriverOpts) > 0 {
			entry["DriverOpts"] = ep.DriverOpts
		}
		// 即使没有任何自定义设置也要保留这个端点：
		// 接在多张网上的容器需要显式列出要接入的每一张网络。
		eps[name] = entry
	}
	if len(eps) == 0 {
		return nil
	}
	return map[string]any{"EndpointsConfig": eps}
}

// isBuiltinNetwork 判断是否 Docker 自带网络。
func isBuiltinNetwork(name string) bool {
	switch name {
	case "bridge", "host", "none", "nat", "default":
		return true
	}
	return false
}

// appendAnonymousVolumes 把隐式卷补成显式绑定，避免重建时数据落到新空卷。
func appendAnonymousVolumes(binds []string, declared []any, actual []dockerx.MountPoint) []string {
	covered := map[string]struct{}{}
	for _, b := range binds {
		if dest := bindDestination(b); dest != "" {
			covered[dest] = struct{}{}
		}
	}
	for _, m := range declared {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if target, ok := mm["Target"].(string); ok && target != "" {
			covered[target] = struct{}{}
		}
	}

	out := make([]string, 0, len(binds)+len(actual))
	out = append(out, binds...)
	for _, mp := range actual {
		if mp.Type != "volume" || mp.Name == "" || mp.Destination == "" {
			continue
		}
		if _, ok := covered[mp.Destination]; ok {
			continue
		}
		out = append(out, mp.Name+":"+mp.Destination)
	}
	return out
}

// mountModes 是 -v 表达式末尾允许出现的挂载选项。
var mountModes = map[string]struct{}{
	"ro": {}, "rw": {}, "z": {}, "Z": {}, "nocopy": {},
	"cached": {}, "delegated": {}, "consistent": {},
	"private": {}, "rprivate": {}, "shared": {}, "rshared": {},
	"slave": {}, "rslave": {}, "unbindable": {}, "runbindable": {},
}

// bindDestination 取出 -v 表达式里的「容器内路径」。
//
// 需要同时应付 `vol:/data`、`vol:/data:ro`、`/data`，
// 以及 Windows 上可能出现的带盘符源路径 `C:\dir:/data`，
// 所以不能简单地按冒号切完固定取第二段——否则覆盖判断会漏，
// 补出一个重复的挂载点，创建阶段会直接报 duplicate mount point。
func bindDestination(b string) string {
	parts := strings.Split(b, ":")
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0] // 只有容器内路径
	case 2:
		return parts[1] // 源:目标
	}
	// 三段及以上：末段可能是挂载选项
	if isMountMode(parts[len(parts)-1]) {
		return parts[len(parts)-2]
	}
	return parts[len(parts)-1]
}

// isMountMode 判断一段是否是挂载选项（ro / rw / z / cached / rprivate ...）。
func isMountMode(part string) bool {
	if part == "" {
		return false
	}
	for _, tok := range strings.Split(part, ",") {
		if _, ok := mountModes[tok]; !ok {
			return false
		}
	}
	return true
}

// cloneMap 通过 JSON 往返做一次深拷贝，避免与原对象共享底层 map。
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return m
	}
	out := map[string]any{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return m
	}
	return out
}

func toStringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func toAnySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func tagOf(ref string) string {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[colon+1:]
	}
	return "latest"
}
