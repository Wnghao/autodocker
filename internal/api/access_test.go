package api

import (
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
)

func port(ip string, private, public int, typ string) dockerx.Port {
	return dockerx.Port{IP: ip, PrivatePort: private, PublicPort: public, Type: typ}
}

// 容器名后面那个箭头要跳到「这个容器的主入口」，跳错了比不跳更烦人。
// 排序口径全部钉在这里。
func TestContainerAccessRanking(t *testing.T) {
	cases := []struct {
		name string
		ctr  dockerx.Container
		want []accessPoint
	}{
		{
			// 常见 web 端口优先于其它端口，无论宿主机端口大小
			name: "web 端口优先",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("", 11434, 11434, "tcp"),
				port("", 80, 8880, "tcp"),
			}},
			want: []accessPoint{{Port: 8880}, {Port: 11434}},
		},
		{
			// 443/8443 那一侧是 https，用 http 打开是一屏乱码
			name: "https 端口标 tls",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("", 80, 8080, "tcp"),
				port("", 443, 8443, "tcp"),
			}},
			want: []accessPoint{{Port: 8443, TLS: true}, {Port: 8080}},
		},
		{
			name: "同为 web 端口按宿主机端口升序",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("", 80, 9080, "tcp"),
				port("", 80, 8080, "tcp"),
			}},
			want: []accessPoint{{Port: 8080}, {Port: 9080}},
		},
		{
			// --net=host：Docker 报的 IP 为空、公网端口与容器内端口相同，照样能点开
			name: "host 网络",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("", 80, 80, "tcp"),
			}},
			want: []accessPoint{{Port: 80}},
		},
		{
			// EXPOSE 但没 -p：宿主机上没有这个端口，链接是死的
			name: "只 EXPOSE 未发布",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				{PrivatePort: 80, Type: "tcp"},
			}},
			want: nil,
		},
		{
			// -p 127.0.0.1:8080:80 只绑回环，从局域网点不开
			name: "只绑回环",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("127.0.0.1", 80, 8080, "tcp"),
			}},
			want: nil,
		},
		{
			name: "udp 打不开网页",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("", 53, 5353, "udp"),
			}},
			want: nil,
		},
		{
			// 真实守护进程对同一个发布端口给两条：0.0.0.0 和 ::（双栈）。
			// 拿 103 上的容器实测出来的，不合并就会出现两个一模一样的链接。
			name: "双栈端口只留一条",
			ctr: dockerx.Container{Running: true, Ports: []dockerx.Port{
				port("0.0.0.0", 5245, 5245, "tcp"),
				port("::", 5245, 5245, "tcp"),
				port("0.0.0.0", 5246, 5246, "tcp"),
				port("::", 5246, 5246, "tcp"),
			}},
			want: []accessPoint{{Port: 5245}, {Port: 5246}},
		},
		{
			name: "停着的容器不给链接",
			ctr: dockerx.Container{Running: false, Ports: []dockerx.Port{
				port("", 80, 8080, "tcp"),
			}},
			want: nil,
		},
	}

	for _, tc := range cases {
		got := containerAccess(tc.ctr)
		if len(got) != len(tc.want) {
			t.Errorf("%s：期望 %d 个候选，实际 %d（%+v）", tc.name, len(tc.want), len(got), got)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s：第 %d 个候选期望 %+v，实际 %+v", tc.name, i, tc.want[i], got[i])
			}
		}
	}
}

// 绑 0.0.0.0 与留空是同一件事，都算「局域网能连」。
func TestContainerAccessWildcards(t *testing.T) {
	for _, ip := range []string{"", "0.0.0.0", "::"} {
		got := containerAccess(dockerx.Container{
			Running: true,
			Ports:   []dockerx.Port{port(ip, 80, 8080, "tcp")},
		})
		if len(got) != 1 {
			t.Errorf("IP %q 应可访问，实际 %+v", ip, got)
		}
	}
}

// Docker 列表接口里 Type 偶尔缺失（老版本守护进程），缺省按 tcp 处理，
// 别把能访问的容器判成没有地址。
func TestContainerAccessEmptyProto(t *testing.T) {
	got := containerAccess(dockerx.Container{
		Running: true,
		Ports:   []dockerx.Port{port("", 80, 8080, "")},
	})
	if len(got) != 1 || got[0].Port != 8080 {
		t.Errorf("Type 为空应照常可访问，实际 %+v", got)
	}
}
