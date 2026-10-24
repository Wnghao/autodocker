// 本机验证面板用的假 Docker 守护进程：只实现面板会调的那几个只读接口。
// 用途是在没有 Docker 的 Windows 机器上，把「容器列表 → 访问地址」这条链路
// 跑通到浏览器里看效果。端口值刻意覆盖几种情形（https、纯 web、非 web、
// 只绑回环、已停止）。
const http = require('node:http');

const cid = (n) => n.repeat(64).slice(0, 64);

const containers = [
  {
    Id: cid('a1'), Names: ['/openlist'], Image: 'openlistteam/openlist:v4.2.8',
    ImageID: 'sha256:1111', State: 'running', Status: 'Up 3 hours',
    Ports: [
      { IP: '0.0.0.0', PrivatePort: 443, PublicPort: 5244, Type: 'tcp' },
      { IP: '::', PrivatePort: 443, PublicPort: 5244, Type: 'tcp' },
      { IP: '0.0.0.0', PrivatePort: 80, PublicPort: 5245, Type: 'tcp' },
      { IP: '::', PrivatePort: 80, PublicPort: 5245, Type: 'tcp' },
    ],
  },
  {
    Id: cid('b2'), Names: ['/xiaomusic'], Image: 'hanxi/xiaomusic:latest',
    ImageID: 'sha256:2222', State: 'running', Status: 'Up 2 days',
    Ports: [{ IP: '0.0.0.0', PrivatePort: 8090, PublicPort: 8090, Type: 'tcp' }],
  },
  {
    Id: cid('c3'), Names: ['/postgres'], Image: 'postgres:16',
    ImageID: 'sha256:3333', State: 'running', Status: 'Up 5 days',
    // 只 EXPOSE 没发布 + 一个非 web 端口
    Ports: [
      { IP: '', PrivatePort: 5432, Type: 'tcp' },
      { IP: '0.0.0.0', PrivatePort: 5432, PublicPort: 15432, Type: 'tcp' },
    ],
  },
  {
    Id: cid('d4'), Names: ['/loopback-only'], Image: 'ghcr.io/foo/bar:1.2.3',
    ImageID: 'sha256:4444', State: 'running', Status: 'Up an hour',
    Ports: [{ IP: '127.0.0.1', PrivatePort: 80, PublicPort: 8080, Type: 'tcp' }],
  },
  {
    Id: cid('e5'), Names: ['/redis-cache'], Image: 'redis:7.4',
    ImageID: 'sha256:5555', State: 'exited', Status: 'Exited (0) 2 hours ago',
    Ports: [{ IP: '0.0.0.0', PrivatePort: 6379, PublicPort: 6379, Type: 'tcp' }],
  },
  {
    Id: cid('f6'), Names: ['/udp-only'], Image: 'metowolf/iptv:1.0',
    ImageID: 'sha256:6666', State: 'running', Status: 'Up 10 minutes',
    Ports: [{ IP: '0.0.0.0', PrivatePort: 41234, PublicPort: 41234, Type: 'udp' }],
  },
];

const server = http.createServer((req, res) => {
  const send = (obj) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify(obj));
  };
  console.log(req.method, req.url);
  const path = req.url.replace(/^\/v[\d.]+/, ''); // 客户端协商版本后会带 /v1.45 前缀
  if (path.startsWith('/containers/json')) return send(containers);
  if (path.startsWith('/images/json')) return send([]);
  if (path === '/version') return send({ ApiVersion: '1.45', Version: '27.1.1' });
  if (path.startsWith('/_ping')) return send({});
  return send([]);
});

server.listen(12375, '127.0.0.1', () => console.log('stub docker on tcp://127.0.0.1:12375'));
