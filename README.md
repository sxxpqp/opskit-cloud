# opskit-cloud

为 [sxxpqp/opskit](https://github.com/sxxpqp/opskit) 配置中的下载源与软件仓库提供云端缓存代理。
采用 Nexus Repository + Caddy HTTPS。缓存首次请求仍需访问上游，不保证突破上游限流或访问权限。

## 覆盖范围

| 内容 | Nexus 仓库或域名 |
|---|---|
| GitHub Release、raw 文件、opskit 自身发布文件 | raw-github / raw-githubusercontent |
| kubeadm / kubelet / kubectl 及摘要文件 | kubernetes-binaries |
| Go SDK 和发布校验元数据 | golang |
| Docker 安装包软件源 | docker-ce |
| Go 模块 / npm / pip / Maven Central | go-proxy / npm-proxy / pypi-proxy / maven-public |
| Docker Hub / registry.k8s.io / quay.io / ghcr.io | dockerhub / k8s / quay / ghcr 子域名 |
| 模板中的阿里云与 Oracle 镜像 | aliyun / oracle 子域名 |

安装文件通过 raw 缓存；容器镜像通过 Docker registry 代理。不会修改数据库、S3、SSH 等业务地址。
私有镜像和受许可限制的 Oracle 镜像仍需合法的上游凭据；本项目不会绕过授权。
Maven 当前覆盖 Central，不包含阿里云 public 聚合仓库中的全部额外制品。
Go 校验数据库访问、系统 apt/yum 源和任意第三方 URL 不等同于这些配置源，未提供通用出口代理。

## 云服务器准备

需要 Linux、Docker Engine + Compose、Python 3.9+，以及足够的内存和缓存磁盘。
先确认云服务器能够访问上述上游；如果云服务器自身无法访问，缓存代理也无法下载。
服务器的公网地址、域名、SSH 信息尚未配置，仓库本身不会自动连接任何服务器。

1. 为主域名及 dockerhub、k8s、quay、ghcr、aliyun、oracle 六个子域名配置 DNS，全部指向云服务器。
2. 安全组放行 80/443。8081 仅绑定本机，不对公网开放；Docker connector 端口不映射宿主机。
3. 下载并校验 Release 包后解压到 /opt/opskit-cloud。首次可先克隆本仓库。
4. 执行以下命令：

```sh
cp .env.example .env
# 编辑 .env：真实域名、ACME 邮箱、客户端公网出口 CIDR。
# CLIENT_CIDRS 支持以空格分隔的多个网段；默认仅 loopback，因此外部客户端不能使用。
# NEXUS_IMAGE、CADDY_IMAGE 应替换为经过验证的固定版本或 sha256 摘要。
docker compose up -d
docker compose logs -f nexus
```

Nexus 需要时间启动。看到启动成功后，读取首次管理员密码：

```sh
docker compose exec nexus cat /nexus-data/admin.password
```

通过 SSH 隧道访问管理界面，完成向导并修改初始密码：

```sh
ssh -L 8081:127.0.0.1:8081 user@your-cloud-server
# 本机浏览器打开 http://127.0.0.1:8081
```

在向导中启用匿名只读下载；公网入口同时受 Caddy 的 CLIENT_CIDRS 限制。
只在受控网络中使用，禁止将 CLIENT_CIDRS 配成任意公网访问。
不要将管理员密码或 .env 提交到公开仓库。

## 初始化全部缓存仓库

在云服务器上执行，密码只通过环境变量提供：

```sh
read -rsp 'Nexus admin password: ' NEXUS_ADMIN_PASSWORD; echo
export NEXUS_ADMIN_PASSWORD
python3 scripts/bootstrap.py
unset NEXUS_ADMIN_PASSWORD
```

脚本使用本机 8081 管理 API，创建缺失仓库，并启用 Docker Bearer Token Realm。
重复执行保留已有同名仓库，不自动覆盖现有配置。
Nexus 各版本 REST 模型可能不同，API 报错时须检查对应版本内置 API 文档，不应忽略错误。
所有缓存持久化在 nexus-data 卷；升级前备份，禁止 docker compose down -v。

## 生成 opskit 配置

本地已有 opskit 完整配置时：

```sh
python3 -m pip install PyYAML==6.0.2
python3 scripts/render_config.py /path/to/opskit/config.yaml \
  --domain cache.example.com --output config.generated.yaml
opskit doctor --config config.generated.yaml --profile local --output json
opskit k8s download --config config.generated.yaml --dry-run
```

输入文件保持不变，输出路径已存在时拒绝覆盖。生成器仅替换 install、kubeblocks 的已知下载地址及 mirrors；
保留 SHA-256、组件版本、业务地址和其他字段值，输出 YAML 不保留注释。
HTTPS 启用证书校验，containerd 使用独立 registry 域名，不再跳过证书检查。
业务容器 image 字符串保持不变，containerd 通过 registry 映射代理。
Docker registry-mirrors 只能透明加速 Docker Hub；其他 registry 若使用 Docker Engine，
需要在镜像引用中显式替换 registry 为对应代理子域名，或使用经验证的其他代理机制。

## 验收

先从 CLIENT_CIDRS 允许的客户端执行：

```sh
curl -fI https://cache.example.com/repository/raw-githubusercontent/sxxpqp/opskit/main/README.md
curl -i https://dockerhub.cache.example.com/v2/
```

Docker /v2/ 可能返回正常的 401 Bearer challenge，不应返回网关 403 或 502。
然后用小镜像实际拉取，再选一个 opskit 组件下载，并核对其原 SHA-256。
同一文件重复下载后在 Nexus 中核对缓存命中。
必须逐一验证目标 registry 的 token、重定向和 blob 下载；配置校验不代替真实拉取测试。

## 自动构建和发版

main 推送、PR 和手动运行自动执行单元测试、Compose/Caddy 配置校验并构建配置包。
推送 v* 标签，在校验成功后自动发布 GitHub Release，附 tar.gz 和 SHA-256 清单。

```sh
git tag v0.1.0
git push origin v0.1.0
```

这是配置项目，发布的是可部署配置包，不重复构建 Nexus/Caddy 的官方镜像。
当前未连接云服务器，自动部署尚未启用。补充 SSH 地址、用户、域名、部署路径后再接入部署任务；
私钥与 known_hosts 应存 GitHub Secrets，配置不得明文提交。

## 上游说明

- [Nexus Docker reverse proxy](https://help.sonatype.com/en/docker-repository-reverse-proxy-strategies.html)
- [Nexus Docker registry](https://help.sonatype.com/en/docker-registry.html)
