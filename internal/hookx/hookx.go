// Package hookx 按 HookSpec 在一次性辅助容器里执行项目自带的检查/更新脚本。
//
// 之所以起辅助容器而不是 exec 进现有容器：update.sh 自己要调
// docker compose build / up，需要 docker.sock；而脚本与项目文件都在宿主机路径上。
// 把「宿主机项目目录 + docker.sock」按原路径挂进一个带齐工具的runner镜像，
// 脚本里的绝对路径、compose 的相对挂载才都能对得上。
package hookx

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
)

// Result 一次脚本执行的结果。
type Result struct {
	Exit   int
	Output string
}

// Executor 执行一次 hook 命令。抽象出来便于 scanner/updater 的测试注入假实现。
type Executor interface {
	Run(ctx context.Context, spec *model.HookSpec, cmd, name string) (*Result, error)
}

// Runner 基于 Docker API 的 Executor 实现。
type Runner struct {
	dk       *dockerx.Client
	defImage string
	log      *slog.Logger
}

// New 构造执行器。defImage 为 spec.image 留空时的全局兜底镜像。
func New(dk *dockerx.Client, defImage string, logger *slog.Logger) *Runner {
	return &Runner{dk: dk, defImage: strings.TrimSpace(defImage), log: logger}
}

// Run 在辅助容器里执行 cmd，阻塞到退出或 ctx 截止。
//
// ctx 超时不会传给守护进程（API 调用取消不等于容器停止），
// 所以取消路径上必须显式 stop + remove，防止留下还在改挂载的孤儿容器。
func (r *Runner) Run(ctx context.Context, spec *model.HookSpec, cmd, name string) (*Result, error) {
	if spec == nil {
		return nil, fmt.Errorf("缺少 hook 配置")
	}
	image := spec.EffectiveImage(r.defImage)
	if image == "" {
		return nil, fmt.Errorf("未指定 hook 运行镜像")
	}

	binds := buildBinds(spec)
	cfg := map[string]any{
		"Image":      image,
		"Cmd":        []string{"sh", "-c", cmd},
		"WorkingDir": spec.Cwd,
		"Labels":     map[string]string{"autodocker.hook": "true"},
	}
	if env := buildEnv(spec.Env); len(env) > 0 {
		cfg["Env"] = env
	}
	hostCfg := map[string]any{"Binds": binds}
	if spec.Network != "" {
		hostCfg["NetworkMode"] = spec.Network
	}

	cName := containerName(name)
	id, err := r.dk.CreateContainer(ctx, cfg, hostCfg, nil, cName)
	if err != nil {
		return nil, fmt.Errorf("创建 hook 辅助容器失败: %w", err)
	}
	defer r.cleanup(id)

	if err := r.dk.StartContainer(ctx, id); err != nil {
		return nil, fmt.Errorf("启动 hook 辅助容器失败: %w", err)
	}

	exit, err := r.waitExit(ctx, id)
	if err != nil {
		return nil, err
	}
	output := ""
	if logs, lerr := r.dk.ContainerLogs(ctx, id); lerr == nil {
		output = tail(logs, 16<<10)
	} else {
		r.log.Warn("读取 hook 输出失败", "container", cName, "err", lerr)
	}
	return &Result{Exit: exit, Output: output}, nil
}

// waitExit 轮询容器状态直到退出。
//
// 不用 /containers/{id}/wait 是因为那要挂一条分钟级的长连接，
// 而 Transport 设了 ResponseHeaderTimeout，长等待会被误判成故障。
func (r *Runner) waitExit(ctx context.Context, id string) (int, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		insp, err := r.dk.InspectContainer(ctx, id)
		if err != nil {
			return -1, fmt.Errorf("跟踪 hook 容器失败: %w", err)
		}
		if insp.State != nil && !insp.State.Running &&
			(insp.State.Status == "exited" || insp.State.Status == "dead") {
			return insp.State.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, fmt.Errorf("hook 执行超时或被取消: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// cleanup 尽力停删辅助容器。
func (r *Runner) cleanup(id string) {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = r.dk.StopContainer(cctx, id, 10)
	_ = r.dk.RemoveContainer(cctx, id, true, false)
}

func buildBinds(spec *model.HookSpec) []string {
	seen := map[string]bool{}
	var binds []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		binds = append(binds, p+":"+p)
	}
	add(spec.Cwd)
	for _, m := range spec.Mounts {
		add(m)
	}
	add("/var/run/docker.sock")
	return binds
}

func buildEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func containerName(name string) string {
	san := strings.Map(func(ch rune) rune {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '.', ch == '_':
			return ch
		default:
			return '-'
		}
	}, name)
	if san == "" {
		san = "task"
	}
	if len(san) > 40 {
		san = san[:40]
	}
	return "autodocker-hook-" + san
}

func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…（前文省略）\n" + s[len(s)-max:]
}
