// Package notify 承担两件事：
//  1. 把扫描发现的更新推送成飞书交互卡片（实现 scanner.Notifier）；
//  2. 接收卡片按钮点击，落更新任务并异步执行，再把结果写回同一张卡片。
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
)

// TargetRef 计算本次更新真正要拉取的镜像引用。
//
// 两种通知的语义并不一样，这是最容易搞错的一处：
//   - 「浮动 tag 内容变更」：容器跑 nginx:latest，远端内容变了 → 还是拉 nginx:latest；
//   - 「仓库出现更高版本 tag」：容器跑 nginx:1.25，仓库里有 1.27 →
//     ImageRef 记录的仍是旧的 1.25，真正要拉的是 1.27。
//
// 因此不能在 new_tag 场景直接使用 ImageRef，否则会变成「把旧版本再拉一遍」的空操作。
func TargetRef(n *model.Notification) string {
	ref, err := registry.ParseReference(n.ImageRef)
	if err != nil {
		return n.ImageRef
	}
	// LatestTag 只在发现更高版本 tag 时才会被写入，且必然与当前 tag 不同
	if n.LatestTag == "" || n.LatestTag == ref.Tag {
		return n.ImageRef
	}
	return ref.WithTag(n.LatestTag)
}

// Service 通知与交互服务。
type Service struct {
	fs  *feishu.Client
	lc  *feishu.LongConnManager
	st  *store.Store
	up  *updater.Updater
	log *slog.Logger

	baseCtx context.Context

	// 按钮回调的观测记录。飞书的「发消息」与「按钮回调」是两条彼此独立的
	// 链路：卡片能收到并不代表按钮点得动。把最近一次回调的时间记下来，
	// 设置页就能给出确定结论，用户不必去翻容器日志猜。
	cbMu     sync.Mutex
	cbAt     time.Time
	cbAction string
	cbCount  int
}

// New 构造通知服务。baseCtx 用于派生异步更新任务的上下文。
func New(ctx context.Context, fs *feishu.Client, st *store.Store, up *updater.Updater, logger *slog.Logger) *Service {
	svc := &Service{fs: fs, st: st, up: up, log: logger, baseCtx: ctx}
	// Service 自己就是卡片回调的处理方，直接把 this 交给长连接管理器
	svc.lc = feishu.NewLongConnManager(ctx, svc, logger)
	return svc
}

// Feishu 暴露底层飞书客户端，供 API 层做配置与测试。
func (s *Service) Feishu() *feishu.Client { return s.fs }

// ApplyFeishu 应用一份飞书配置并确保长连接相应收敛。
//
// 网页设置保存后由 API 层调用；凭据留空即视为关闭飞书通知，
// 长连接也会一并停掉，避免拿着无效凭据反复重连刷日志。
func (s *Service) ApplyFeishu(appID, appSecret, chatID string) {
	s.fs.SetCredentials(appID, appSecret)
	s.fs.SetChatID(chatID)
	s.lc.Ensure(appID, appSecret)
}

// LongConnRunning 返回卡片回调长连接是否已就绪。
func (s *Service) LongConnRunning() bool { return s.lc.Running() }

// Close 关闭长连接，用于进程退出前的收尾。
func (s *Service) Close() { s.lc.Stop() }

// recordCallback 记下一次按钮回调，返回其时间戳。
func (s *Service) recordCallback(action string) time.Time {
	now := time.Now()
	s.cbMu.Lock()
	s.cbAt, s.cbAction = now, action
	s.cbCount++
	s.cbMu.Unlock()
	return now
}

// CallbackState 返回最近一次按钮回调的观测结果。
// at 为零值表示服务启动以来还没收到过任何回调 —— 这正是设置页要显示的信息。
func (s *Service) CallbackState() (at time.Time, action string, count int) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	return s.cbAt, s.cbAction, s.cbCount
}

// Notify 实现 scanner.Notifier：发送更新卡片并返回 message_id。
func (s *Service) Notify(ctx context.Context, n *model.Notification) (string, error) {
	if !s.fs.Configured() {
		s.log.Debug("飞书未配置，通知仅落库", "ref", n.ImageRef)
		return "", nil
	}
	card := feishu.BuildUpdateCard(n)
	msgID, err := s.fs.SendCard(ctx, "", card)
	if err != nil {
		return "", fmt.Errorf("发送飞书卡片失败: %w", err)
	}
	s.log.Info("已推送更新提醒", "ref", n.ImageRef, "message_id", msgID)
	return msgID, nil
}

// HandleCardAction 实现 feishu.ActionHandler：处理按钮点击。
//
// 必须在 3 秒内返回，因此这里只创建任务并立刻返回「进行中」的卡片，
// 真正的重建放到后台 goroutine 里跑。
func (s *Service) HandleCardAction(ctx context.Context, v feishu.CardValue) (string, map[string]any) {
	at := s.recordCallback(v.Action)

	// 测试卡片上的按钮不关联任何通知，必须在查库**之前**处理，
	// 否则会掉进下面「通知已失效」那条分支 —— 表现为点了按钮提示
	// 「通知已失效」，让人以为是自己配错了。
	if v.Action == feishu.ActionTest {
		s.log.Info("收到飞书测试卡片回调，按钮链路正常")
		return "回调已收到", feishu.BuildTestDoneCard(at)
	}

	n, err := s.st.GetNotification(v.NID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "通知已失效", nil
		}
		s.log.Warn("读取通知失败", "nid", v.NID, "err", err)
		return "服务异常", nil
	}

	switch v.Action {
	case feishu.ActionIgnore:
		if err := s.st.SetNotificationCardState(n.ID, model.CardIgnored); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
		// 光把这条通知标成「已忽略」是拦不住下一张卡片的：扫描只看远端摘要变没变，
		// 下一轮照样判成「有更新」并新建一条通知。必须把「用户已放行这一版」
		// 记到监控项上，卡片上那句「同一个版本不会再次提醒」才是真的。
		s.rememberIgnoredDigest(n)
		return "已忽略该版本", feishu.BuildStateCard(n, model.CardIgnored,
			"已忽略本版本，同一个版本不会再次提醒。\n后续出现更高的版本时仍会通知。")

	case feishu.ActionSnooze:
		if err := s.st.SetNotificationCardState(n.ID, model.CardSnoozed); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		return "已设为稍后", feishu.BuildStateCard(n, model.CardSnoozed,
			"已记录。本轮巡检不会重复提醒，下次巡检如有更新会重新推送。")

	case feishu.ActionUpdate:
		return s.startUpdate(n)

	default:
		return "未知操作", nil
	}
}

// rememberIgnoredDigest 把「用户放行的这次远端内容」记到监控项上。
//
// 记的是库里**当前**的远端摘要，而不是通知里那份快照：用户点按钮时想放行的是
// 「现在仓库里这一版」，以最新观测为准更稳妥（期间要是又推了一版，下一轮该提醒还得提醒）。
// 记录失败不影响回调返回 —— 按钮已经点了，卡片状态也改了，这里只是尽力而为，
// 真失败了顶多下次再被提醒一遍。
func (s *Service) rememberIgnoredDigest(n *model.Notification) {
	if n.ContainerID == "" {
		return
	}
	img, err := s.st.FindImage(n.ImageRef, n.ContainerID)
	if err != nil {
		s.log.Warn("读取监控项失败，无法记录已忽略版本", "ref", n.ImageRef, "err", err)
		return
	}
	if img.RemoteDigest == "" {
		return
	}
	if err := s.st.SetImageIgnoredDigest(img.ID, img.RemoteDigest); err != nil {
		s.log.Warn("记录已忽略版本失败", "ref", n.ImageRef, "err", err)
	}
}

// startUpdate 处理飞书按钮点击：创建任务并返回「进行中」卡片。
func (s *Service) startUpdate(n *model.Notification) (string, map[string]any) {
	if n.CardState == model.CardUpdating {
		return "更新已在进行中", nil
	}
	taskID, err := s.TriggerUpdate(n, "feishu")
	if err != nil {
		return "无法更新", feishu.BuildStateCard(n, model.CardFailed, err.Error())
	}
	return "已开始更新", feishu.BuildStateCard(n, model.CardUpdating,
		fmt.Sprintf("已受理，正在拉取镜像并重建容器（任务 #%d）。\n完成后本卡片会自动更新结果，期间无需其他操作。", taskID))
}

// TriggerUpdate 创建并异步执行一次更新任务，返回任务 ID。
// 飞书按钮与 Web 页面共用这一入口，保证两条路径行为一致。
func (s *Service) TriggerUpdate(n *model.Notification, triggeredBy string) (int64, error) {
	if n.ContainerID == "" {
		return 0, errors.New("该监控项没有关联的本地容器，无法执行更新")
	}
	if s.up.Busy() {
		return 0, errors.New("已有更新任务正在执行，请稍后重试")
	}
	if busy, err := s.st.HasRunningTask(n.ContainerID); err == nil && busy {
		return 0, errors.New("该容器已有进行中的更新任务")
	}

	task := &model.Task{
		NotificationID: n.ID,
		ImageRef:       TargetRef(n),
		ContainerID:    n.ContainerID,
		ContainerName:  n.ContainerName,
		TriggeredBy:    triggeredBy,
	}
	taskID, err := s.st.CreateTask(task)
	if err != nil {
		s.log.Error("创建更新任务失败", "err", err)
		return 0, fmt.Errorf("创建更新任务失败: %w", err)
	}
	if n.ID > 0 {
		if err := s.st.SetNotificationCardState(n.ID, model.CardUpdating); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
	}

	go s.runUpdate(n, taskID, triggeredBy)
	return taskID, nil
}

// runUpdate 在后台执行更新，并把最终结果写回卡片。
func (s *Service) runUpdate(n *model.Notification, taskID int64, triggeredBy string) {
	ctx, cancel := context.WithTimeout(s.baseCtx, 45*time.Minute)
	defer cancel()

	target := TargetRef(n)
	s.log.Info("开始执行更新", "task", taskID, "container", n.ContainerName,
		"from", n.ImageRef, "target", target, "by", triggeredBy)

	err := s.up.Run(ctx, updater.Request{
		TaskID:        taskID,
		ImageRef:      target,
		ContainerID:   n.ContainerID,
		ContainerName: n.ContainerName,
		TriggeredBy:   triggeredBy,
	})

	state := model.CardDone
	detail := "容器已在新镜像上重建完成，原有挂载、端口与网络配置保持不变。"
	if err != nil {
		state = model.CardFailed
		detail = "更新失败：" + err.Error()
		if t, terr := s.st.GetTask(taskID); terr == nil && t.Status == model.TaskRolledBack {
			state = model.CardRollback
			detail = "新容器未通过健康检查，已自动回滚到原版本（原容器已恢复运行）。\n原因：" + err.Error()
		}
	}

	if n.ID > 0 {
		if serr := s.st.SetNotificationCardState(n.ID, state); serr != nil {
			s.log.Warn("写回通知状态失败", "err", serr)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
	}

	// 原地替换卡片，形成可追溯的操作记录
	if n.FeishuMessageID == "" || !s.fs.Configured() {
		return
	}
	pushCtx, cancelPush := context.WithTimeout(s.baseCtx, 30*time.Second)
	defer cancelPush()
	if perr := s.fs.PatchCard(pushCtx, n.FeishuMessageID, feishu.BuildStateCard(n, state, detail)); perr != nil {
		s.log.Warn("更新飞书卡片失败", "message_id", n.FeishuMessageID, "err", perr)
	}
}

// SendTestCard 发送一张测试卡片，用于部署后验证配置。
func (s *Service) SendTestCard(ctx context.Context) error {
	if !s.fs.Configured() {
		return errors.New("飞书应用未配置，请先填写 App ID 与 App Secret")
	}
	chatID := s.fs.DefaultChatID()
	if chatID == "" {
		return errors.New("尚未配置接收群 chat_id")
	}
	_, err := s.fs.SendCard(ctx, chatID, feishu.BuildTestCard(chatID))
	return err
}
