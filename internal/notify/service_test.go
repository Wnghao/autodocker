package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/notify/feishu"
)

// quietService 造一个只带 logger 的 Service，刻意不装配 store / updater / 飞书客户端。
//
// 这是有意为之：测试卡片的回调不关联任何通知，必须在碰数据库**之前**就被受理。
// 一旦有人把 ActionTest 分支挪到 GetNotification 后面，下面这个测试立刻会因为
// nil store 而 panic —— 正好把这类回归卡死在提交前。
func quietService() *Service {
	return &Service{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestHandleCardActionAcceptsTestCallback(t *testing.T) {
	s := quietService()

	toast, card := s.HandleCardAction(context.Background(), feishu.CardValue{Action: feishu.ActionTest})
	if toast == "" {
		t.Fatal("测试回调应返回一句 toast 反馈，否则群里点了没有任何提示")
	}
	if card == nil {
		t.Fatal("测试回调应返回回执卡片，让群里看到这一下点击真的生效了")
	}

	at, action, count := s.CallbackState()
	if count != 1 {
		t.Fatalf("回调计数应为 1，得到 %d", count)
	}
	if action != feishu.ActionTest {
		t.Fatalf("记录的 action 应为 %q，得到 %q", feishu.ActionTest, action)
	}
	if at.IsZero() || time.Since(at) > time.Minute {
		t.Fatalf("回调时间戳不合理: %v", at)
	}
}

// 设置页靠「有没有回调时间」区分「从未验证过」与「已验证」，初始必须是零值。
func TestCallbackStateZeroBeforeAnyCallback(t *testing.T) {
	at, action, count := quietService().CallbackState()
	if !at.IsZero() || action != "" || count != 0 {
		t.Fatalf("初始状态应为零值，得到 %v / %q / %d", at, action, count)
	}
}
