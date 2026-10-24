// 这一份管「compose 化容器不要脱离 compose 管辖」。
//
// 逐容器重建（updater.Run 的主路径）是按 inspect 到的配置造一个新容器，
// 新容器身上没有 compose 标签 —— 项目从此认不出它：`compose ps` 看不到，
// 下次 `compose up` 又造一个同名服务，撞名之后两头都管不了。
// 对家里这种「几乎所有容器都是 1Panel 用 compose 起起来的」环境，
// 每次更新都在制造一个孤儿，所以能被 compose 管的就交给 compose 重建。
package updater

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hookx"
	"github.com/haihaipypy/autodocker/internal/model"
)

// composeFallbackExit 是辅助脚本自己约定的退出码：
// 表示「compose 这条路走不通」，且**此时脚本还没碰任何容器**，
// 调用方可以安全地回落到逐容器重建。
const composeFallbackExit = 20

// composeOverrideFile 是那段临时 override 落在辅助容器里的路径，只为了改写 image。
//
// 路径必须是固定常量：compose 会把本次用到的每个 -f 都写进
// com.docker.compose.project.config_files 标签，下一轮巡检就得靠这个名字
// 把它从「项目自己的文件」里认出来剔掉 —— 它只存在于当时那个辅助容器里，
// 宿主机上根本没有，留着它下次预检必然读不通。
const composeOverrideFile = "/tmp/ad-image.yml"

// composeTarget 一个容器所属的 compose 服务坐标，全部来自 compose 自己写的标签。
type composeTarget struct {
	project string
	service string
	workDir string
	files   []string
}

// detectCompose 从容器标签还原 compose 归属，不是 compose 起的就返回 nil。
//
// 少任何一项坐标都不返回半份配置：项目名/服务名决定重建谁，
// 工作目录与文件决定用哪份配置，缺一个就没法安全地交给 compose。
func detectCompose(insp *dockerx.Inspect) *composeTarget {
	lb := labelMap(insp.Config["Labels"])
	if strings.TrimSpace(lb["com.docker.compose.oneoff"]) == "True" {
		return nil // docker compose run 出来的一次性容器，没有「服务」可重建
	}
	t := &composeTarget{
		project: strings.TrimSpace(lb["com.docker.compose.project"]),
		service: strings.TrimSpace(lb["com.docker.compose.service"]),
		workDir: strings.TrimSpace(lb["com.docker.compose.project.working_dir"]),
	}
	for _, f := range strings.Split(lb["com.docker.compose.project.config_files"], ",") {
		f = strings.TrimSpace(f)
		// 上一次 compose 更新留下的临时文件：宿主机上不存在，
		// 当成项目配置传给 compose，下次预检就再也读不通了
		if f == "" || f == composeOverrideFile {
			continue
		}
		t.files = append(t.files, f)
	}
	if t.project == "" || t.service == "" || len(t.files) == 0 {
		return nil
	}
	// 辅助容器要按宿主机绝对路径挂载项目目录，相对路径挂不出东西来
	if !strings.HasPrefix(t.workDir, "/") {
		return nil
	}
	return t
}

// extraMounts 列出项目目录之外的 compose 文件所在目录。
//
// 挂目录而不是文件：路径写错时 Docker 会在宿主机上按「目录」补出来，
// 挂一个不存在的文件等于在人家机器上造一个同名空目录。
// 这里用 path 而不是 filepath：标签里的路径永远是宿主机的 Linux 路径，
// 在 Windows 上跑测试时 filepath.Dir 会把 /opt/x 拆成 \opt\x。
func (t *composeTarget) extraMounts() []string {
	seen := map[string]bool{t.workDir: true}
	var out []string
	for _, f := range t.files {
		if !strings.HasPrefix(f, "/") {
			continue // 相对路径由 --project-directory 兜住，不用额外挂
		}
		d := path.Dir(f)
		if d == t.workDir || strings.HasPrefix(d, t.workDir+"/") || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// composeArgs 是每次调用 compose 都要带上的公共参数。
//
// -p 与 --project-directory 必须显式给：目录名和 1Panel 的项目名经常不一致
// （应用在 /opt/1panel/apps/openlist/openlist，项目名却是 openlist），
// 让 compose 自己按目录猜会分裂出第二个项目。
func (t *composeTarget) args(overrideFile string) string {
	parts := []string{"--project-directory " + shq(t.workDir), "-p " + shq(t.project)}
	for _, f := range t.files {
		parts = append(parts, "-f "+shq(f))
	}
	if overrideFile != "" {
		parts = append(parts, "-f "+shq(overrideFile))
	}
	return strings.Join(parts, " ")
}

// composeScript 生成辅助容器里跑的那段 shell：
// 先预检（compose CLI 在不在、这份配置读不读得通），再把「这一个服务」的镜像
// 换成目标引用后 up -d。
//
// 镜像只经由 override 文件改写，项目自带的 compose 文件一个字都不动 ——
// 这是本服务唯一被允许做的事：换镜像，其余配置一律以项目自己的文件为准。
func (t *composeTarget) composeScript(imageRef string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then\n")
	b.WriteString("  echo '辅助镜像里没有 docker compose，请把 HOOK_IMAGE 换成自带 compose 插件的 runner'\n")
	b.WriteString("  exit " + strconv.Itoa(composeFallbackExit) + "\nfi\n")
	b.WriteString("if ! docker compose " + t.args("") + " config --quiet 2>/tmp/ad-compose-err; then\n")
	b.WriteString("  echo 'compose 配置读不通：' ; sed -n '1,20p' /tmp/ad-compose-err\n")
	b.WriteString("  exit " + strconv.Itoa(composeFallbackExit) + "\nfi\n")
	b.WriteString("cat > " + composeOverrideFile + " <<'YAML'\n")
	fmt.Fprintf(&b, "services:\n  %q:\n    image: %q\n", t.service, imageRef)
	b.WriteString("YAML\n")
	b.WriteString("docker compose " + t.args(composeOverrideFile) + " up -d --no-color " + shq(t.service) + "\n")
	return b.String()
}

// rollbackRef 把旧镜像钉住用的引用（repo 与 tag 分开给 dockerx.TagImage）。
func (t *composeTarget) rollbackRef(imageID string) (repo, tag, full string) {
	repo = "autodocker-rollback"
	tag = sanitizeRefComponent(t.project + "-" + t.service + "-" + shortImageID(imageID))
	return repo, tag, repo + ":" + tag
}

// sanitizeRefComponent 把 Docker 标签里不允许的字符换成短横。
func sanitizeRefComponent(s string) string {
	var b strings.Builder
	prevDash := false
	for _, ch := range s {
		ok := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.'
		switch {
		case ok:
			b.WriteRune(ch)
			prevDash = false
		case ch >= 'A' && ch <= 'Z':
			b.WriteRune(ch + 32)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	return strings.Trim(out, "-._")
}

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// composeUpgrade 把这个服务交回 compose 重建，成功与否都写进任务。
//
// handled=false 只有一种含义：compose 这条路预检就没走通（辅助容器里没有 compose，
// 或那份配置读不出来），而且**还没动过任何容器**，调用方应当回落到逐容器重建。
func (u *Updater) composeUpgrade(
	ctx context.Context,
	req Request,
	insp *dockerx.Inspect,
	t *composeTarget,
	wasRunning bool,
	opt Options,
	logf func(string, ...any),
) (bool, error) {
	name := insp.Name
	spec := &model.HookSpec{Enabled: true, Cwd: t.workDir, Mounts: t.extraMounts()}

	// 回滚把手。compose 会自己停掉并删掉旧容器，「重命名旧容器留住现场」那套对它不适用；
	// 而新镜像拉下来之后，旧那份虽然还在磁盘上，却已经没有名字可指。
	// 所以在这里按镜像 ID 给它钉一个 tag，健康检查不过时还能让 compose 退回这一版。
	repo, tag, ref := t.rollbackRef(insp.Image)
	pinned := false
	if insp.Image != "" {
		if perr := u.dk.TagImage(ctx, insp.Image, repo, tag); perr == nil {
			pinned = true
		} else {
			logf("警告：%v（本次更新没有自动回滚的把手）", perr)
		}
	}

	res, rerr := u.runCompose(ctx, opt, spec, t, req.ImageRef, name)
	if rerr != nil {
		// 辅助容器压根没跑起来，现场没动过：回落到原路径，让更新要么落成要么干净失败
		logf("compose 重建未能执行（%v）", rerr)
		if pinned {
			_ = u.dk.RemoveImage(ctx, ref, false)
		}
		return false, nil
	}
	for _, line := range lastLines(res.Output, 20) {
		logf("%s", line)
	}
	if res.Exit == composeFallbackExit {
		if pinned {
			_ = u.dk.RemoveImage(ctx, ref, false)
		}
		return false, nil
	}
	if res.Exit != 0 {
		msg := fmt.Sprintf("compose 重建失败（退出码 %d）：%s", res.Exit, briefTail(res.Output, 400))
		if pinned && opt.RollbackOnFailure && u.composeRollback(ctx, opt, spec, t, ref, name, logf) {
			_ = u.st.FinishTask(req.TaskID, model.TaskRolledBack, "", msg)
			return true, errors.New(msg)
		}
		logf("compose 重建失败，旧容器可能已被 compose 删除，需要人工确认服务状态")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", msg)
		return true, errors.New(msg)
	}

	newC, ok := u.findContainerByName(ctx, name)
	if !ok {
		msg := "compose 执行完成，但按名字找不到该服务的容器，请检查项目里的 container_name / 服务名是否被改过"
		logf("%s", msg)
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", msg)
		return true, errors.New(msg)
	}

	// compose 的 up 一定会把服务带起来；原本停着的容器不能因为「更新」被顺手启动
	if !wasRunning {
		logf("该服务原本未运行，重建后再停回原状态")
		if serr := u.dk.StopContainer(ctx, newC.ID, opt.StopTimeoutSec); serr != nil {
			logf("警告：停止新容器失败（%v）", serr)
		}
	} else {
		hok, hmsg := u.waitHealthy(ctx, newC.ID, opt.HealthCheckWait, logf)
		if !hok {
			if pinned && opt.RollbackOnFailure && u.composeRollback(ctx, opt, spec, t, ref, name, logf) {
				_ = u.st.FinishTask(req.TaskID, model.TaskRolledBack, "", hmsg)
				return true, errors.New(hmsg)
			}
			logf("新容器未通过健康检查（%s），且没有可用的自动回滚", hmsg)
			_ = u.st.FinishTask(req.TaskID, model.TaskFailed, newC.ID, hmsg)
			return true, errors.New(hmsg)
		}
		logf("健康检查通过：%s", hmsg)
	}

	// 新版站稳了，回滚用的 tag 就没用了；回滚成功时相反，那个 tag 正是容器现在的镜像
	if pinned {
		if uerr := u.dk.RemoveImage(ctx, ref, false); uerr != nil {
			logf("提示：回滚标签 %s 未能清理（%v），可手动 docker rmi 删掉", ref, uerr)
		}
	}
	logf("更新完成：%s 由 compose 重建，仍在项目 %s 的管辖内", name, t.project)
	u.refreshAfterUpgrade(ctx, req.ImageRef, req.ContainerID, newC.ID, name)
	_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, newC.ID, "")
	return true, nil
}

// runCompose 起一个辅助容器跑 compose，脚本自己负责预检与退出码约定。
func (u *Updater) runCompose(
	ctx context.Context,
	opt Options,
	spec *model.HookSpec,
	t *composeTarget,
	imageRef, name string,
) (*hookx.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.UpdateTimeoutSec())*time.Second)
	defer cancel()
	return opt.Hook.Run(runCtx, spec, t.composeScript(imageRef), name)
}

// composeRollback 用同一份 compose 配置把镜像换回旧版本。
//
// 只认「退回旧镜像」这一件事：配置仍然是项目自己那份，改回来的也只有 image，
// 所以回滚后的容器照样带着 compose 标签，不会变成一个管不了的孤儿。
func (u *Updater) composeRollback(
	ctx context.Context,
	opt Options,
	spec *model.HookSpec,
	t *composeTarget,
	ref, name string,
	logf func(string, ...any),
) bool {
	logf("执行回滚：用 compose 退回旧镜像 %s", ref)
	res, err := u.runCompose(ctx, opt, spec, t, ref, name)
	if err != nil {
		logf("回滚未能执行（%v），请手动回退", err)
		return false
	}
	for _, line := range lastLines(res.Output, 10) {
		logf("%s", line)
	}
	if res.Exit != 0 {
		logf("compose 回滚失败（退出码 %d），请手动重新部署旧版本", res.Exit)
		return false
	}
	logf("回滚完成：%s 已恢复在旧镜像上运行（compose 配置未改动）", name)
	return true
}

func labelMap(v any) map[string]string {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		for k, s := range m {
			out[k] = s
		}
	case map[string]any:
		for k, val := range m {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}
