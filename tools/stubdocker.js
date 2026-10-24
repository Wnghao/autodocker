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
    Labels: { 'com.docker.compose.project': 'nas-apps', 'com.docker.compose.service': 'openlist', 'com.docker.compose.oneoff': 'False',
      'com.docker.compose.project.working_dir': '/opt/1panel/docker/compose/nas-apps',
      'com.docker.compose.project.config_files': '/opt/1panel/docker/compose/nas-apps/docker-compose.yml' },
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
    Labels: { 'com.docker.compose.project': 'music', 'com.docker.compose.service': 'xiaomusic', 'com.docker.compose.oneoff': 'False',
      'com.docker.compose.project.working_dir': '/opt/1panel/docker/compose/music',
      'com.docker.compose.project.config_files': '/opt/1panel/docker/compose/music/docker-compose.yml' },
    Ports: [{ IP: '0.0.0.0', PrivatePort: 8090, PublicPort: 8090, Type: 'tcp' }],
  },
  {
    Id: cid('c3'), Names: ['/postgres'], Image: 'postgres:16',
    ImageID: 'sha256:3333', State: 'running', Status: 'Up 5 days',
    Labels: { 'com.docker.compose.project': 'nas-apps', 'com.docker.compose.service': 'postgres', 'com.docker.compose.oneoff': 'False',
      'com.docker.compose.project.working_dir': '/opt/1panel/docker/compose/nas-apps',
      'com.docker.compose.project.config_files': '/opt/1panel/docker/compose/nas-apps/docker-compose.yml' },
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
    Labels: { 'com.docker.compose.project': 'music', 'com.docker.compose.service': 'redis', 'com.docker.compose.oneoff': 'False',
      'com.docker.compose.project.working_dir': '/opt/1panel/docker/compose/music',
      'com.docker.compose.project.config_files': '/opt/1panel/docker/compose/music/docker-compose.yml' },
    Ports: [{ IP: '0.0.0.0', PrivatePort: 6379, PublicPort: 6379, Type: 'tcp' }],
  },
  {
    Id: cid('f6'), Names: ['/udp-only'], Image: 'metowolf/iptv:1.0',
    ImageID: 'sha256:6666', State: 'running', Status: 'Up 10 minutes',
    Ports: [{ IP: '0.0.0.0', PrivatePort: 41234, PublicPort: 41234, Type: 'udp' }],
  },
  {
    // 单服务项目：不该被折叠成组，用来盯住「>=2 个容器才分组」这条规则。
    Id: cid('g7'), Names: ['/minio'], Image: 'minio/minio:latest',
    ImageID: 'sha256:7777', State: 'running', Status: 'Up 4 hours',
    Labels: { 'com.docker.compose.project': 'solo-app', 'com.docker.compose.service': 'minio', 'com.docker.compose.oneoff': 'False',
      'com.docker.compose.project.working_dir': '/opt/1panel/docker/compose/solo-app',
      'com.docker.compose.project.config_files': '/opt/1panel/docker/compose/solo-app/docker-compose.yml' },
    Ports: [{ IP: '0.0.0.0', PrivatePort: 9001, PublicPort: 9001, Type: 'tcp' }],
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
  // 单个容器详情：compose 文件弹窗要靠 Config.Labels 里的 working_dir / config_files 定位文件
  const insp = path.match(/^\/containers\/([^/]+)\/json$/);
  if (insp) {
    const c = containers.find((x) => x.Id === insp[1]);
    if (!c) return send({ message: 'No such container: ' + insp[1] });
    return send({
      Id: c.Id,
      Name: c.Names[0],
      Config: { Labels: c.Labels || {} },
      HostConfig: {},
      State: { Running: c.State === 'running', Status: c.State, ExitCode: 0 },
      Mounts: [],
    });
  }
  if (path.startsWith('/images/json')) return send([]);
  if (path === '/version') return send({ ApiVersion: '1.45', Version: '27.1.1' });
  if (path.startsWith('/_ping')) return send({});
  return send([]);
});

server.listen(12375, '127.0.0.1', () => console.log('stub docker on tcp://127.0.0.1:12375'));
