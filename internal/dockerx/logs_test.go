package dockerx

import (
	"encoding/binary"
	"strings"
	"testing"
)

// 非 TTY 容器的日志是多路复用帧：8 字节头 + 负载。
// 帧头里的长度是小端还是大端、stdout 与 stderr 怎么分，都得在这儿钉死 ——
// 脚本失败时页面上那行「原因」全靠它，解错就是满屏乱码。
func TestDemuxStreamFrames(t *testing.T) {
	var buf []byte
	frame := func(kind byte, s string) {
		hdr := make([]byte, 8)
		hdr[0] = kind
		binary.BigEndian.PutUint32(hdr[4:8], uint32(len(s)))
		buf = append(buf, hdr...)
		buf = append(buf, s...)
	}
	frame(1, "重建镜像\n")
	frame(2, "compose build 失败\n")
	frame(1, "已回滚\n")

	got := DemuxStream(buf)
	want := "重建镜像\ncompose build 失败\n已回滚\n"
	if got != want {
		t.Fatalf("demux 结果不对:\n得到 %q\n期望 %q", got, want)
	}
	// 帧头字节绝不能混进文本
	for _, banned := range []string{"\x00", "\x01", "\x02"} {
		if strings.ContainsRune(got, rune(banned[0])) {
			t.Fatalf("输出里混进了控制字节: %q", got)
		}
	}
}

// TTY 容器没有帧头，整段就是原始文本；这种不能按帧去解，否则第一行就被当长度吃掉。
func TestDemuxStreamPassthroughTTY(t *testing.T) {
	raw := "普通输出：build 用了 42 秒\n"
	if got := DemuxStream([]byte(raw)); got != raw {
		t.Fatalf("TTY 输出应原样返回，得到 %q", got)
	}
	if got := DemuxStream(nil); got != "" {
		t.Fatalf("空日志应返回空串，得到 %q", got)
	}
}

// 读取被截断时不能丢内容：把尾部原样附上，好过报「没有输出」。
func TestDemuxStreamTruncated(t *testing.T) {
	hdr := make([]byte, 8)
	hdr[0] = 1
	binary.BigEndian.PutUint32(hdr[4:8], 4096) // 声明 4096 字节，实际只有一小段
	buf := append(hdr, []byte("末尾半句")...)

	got := DemuxStream(buf)
	if !strings.Contains(got, "末尾半句") {
		t.Fatalf("截断帧的负载应被保留，得到 %q", got)
	}
}
