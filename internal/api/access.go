package api

import (
	"context"
	"sort"

	"github.com/haihaipypy/autodocker/internal/dockerx"
)

// accessPoint 是「点开这个容器」的一个候选地址。
//
// 只回端口，不回主机名：这个服务装在 NAS 上，面板可能用局域网 IP、域名或反代打开，
// 只有浏览器知道自己此刻用的哪个地址，所以由前端拿 location.hostname 去拼。
type accessPoint struct {
	Port int  `json:"port"`
	TLS  bool `json:"tls"` // 容器内那一侧是 https（443/8443 这类端口）
}

// webPrivatePorts 是常见 Web 服务在容器内监听的端口，按「像不像主入口」排序。
// 一个容器发布好几个端口时，靠前的这个才是用户想打开的那个页面
// —— 比如 qlwo 同时开 11434（模型 API）和 3000（Web 控制台）。
var webPrivatePorts = []int{443, 8443, 9443, 80, 8080, 3000, 5000, 8000, 8081, 9000, 9090}

// tlsPrivatePorts 容器内这几个端口基本只跑 https，用 http 打开会得到一屏乱码。
var tlsPrivatePorts = map[int]bool{443: true, 8443: true, 9443: true}

// containerAccess 算出一个容器对外可访问的端口，按推荐程度排序。
// 停着的容器一个都不给：链接点开是「拒绝连接」，不如没有。
func containerAccess(c dockerx.Container) []accessPoint {
	if !c.Running {
		return nil
	}
	type cand struct {
		public, private int
	}
	var list []cand
	for _, p := range c.Ports {
		if p.Type == "udp" {
			continue // UDP 没有能点开的网页
		}
		if p.PublicPort <= 0 || p.PrivatePort <= 0 {
			continue // 只是 EXPOSE，没有真发布到宿主机
		}
		// --p=127.0.0.1:8080:80 这种只绑回环的，从局域网点不开。
		// host 网络下 Docker 填的 IP 是空的，空串算「绑所有网卡」，不能当回环。
		if p.IP == "127.0.0.1" || p.IP == "::1" {
			continue
		}
		list = append(list, cand{public: p.PublicPort, private: p.PrivatePort})
	}

	rank := func(private int) int {
		for i, w := range webPrivatePorts {
			if w == private {
				return i
			}
		}
		return len(webPrivatePorts)
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i].private), rank(list[j].private)
		if ri != rj {
			return ri < rj
		}
		return list[i].public < list[j].public
	})

	// 发布到所有网卡时，Docker 会为同一个端口给两条：
	// 0.0.0.0 一条、:: 一条（v4/v6 双栈）。对「点开哪个地址」来说是同一条。
	seen := make(map[accessPoint]bool, len(list))
	out := make([]accessPoint, 0, len(list))
	for _, v := range list {
		p := accessPoint{Port: v.public, TLS: tlsPrivatePorts[v.private]}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// accessByContainer 一次列举，得到「容器 ID → 访问地址候选」。
// 读不到 Docker 时返回空表：列表照常显示，只是没有可点开的链接。
func (s *Server) accessByContainer(ctx context.Context) map[string][]accessPoint {
	containers, err := s.dk.ListContainers(ctx)
	if err != nil {
		s.log.Warn("列举容器端口失败，列表将不带访问地址", "err", err)
		return nil
	}
	out := make(map[string][]accessPoint, len(containers))
	for _, c := range containers {
		if acc := containerAccess(c); len(acc) > 0 {
			out[c.ID] = acc
		}
	}
	return out
}
