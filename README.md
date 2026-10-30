# opskit-cloud

一个 Go 编写的下载与镜像转发服务。客户端 → Go 服务 → 上游。
不依赖 Nexus、Caddy、Docker，也不提供本地磁盘缓存。

## 1. 配置放在哪里

复制 config.example.yaml 为 config.yaml，先修改：

```yaml
listen: ":8080"                         # 云服务器监听端口
public_url: "http://你的服务器IP:8080"   # 客户端能访问的地址
allow_cidrs: ["你的公网出口IP/32"]       # 哪些客户端可以使用
timeout: "15m"                          # 单次下载最长时间
```

downloads 是下载源，registries 是镜像源。配置文件有完整默认值和中文注释。
不填写任意开放网段；代理只接受白名单客户端的 GET/HEAD，不允许上传。
使用请求连接的真实来源 IP 判断，不信任客户端自行发送的 X-Forwarded-For。
若另加负载均衡，应在网络层限制入口，不能把共享负载均衡 IP 当成所有用户的鉴权。
/healthz 不检查白名单，只表示进程能响应，不能证明上游可达。

## 2. 启动

```sh
go build -o opskit-cloud .
./opskit-cloud init --output config.yaml
# 编辑 config.yaml 后启动
./opskit-cloud serve --config config.yaml
```

也可以通过参数生成配置：

```sh
./opskit-cloud init \
  --public-url https://cache.example.com \
  --allow-cidrs '203.0.113.10/32,198.51.100.0/24' \
  --upstream github=https://github.com/ \
  --output config.yaml
```

init 拒绝覆盖已有文件。serve 支持 --listen、--public-url 临时覆盖，以及 --check 只校验。
直接由 Go 提供 HTTPS 时，在 YAML 填 tls_cert、tls_key；证书文件需运行账号可读。
CI 默认部署 HTTP :8080。公网生产使用应配置可信 TLS，避免将私有仓库凭据通过 HTTP 发送。
只修改 public_url 不会自动启用 TLS；也不自动申请证书。

## 3. 转发地址

| 内容 | 入口路径 |
|---|---|
| GitHub Release / raw 文件 | /github/ 与 /raw/ |
| Kubernetes 二进制 | /kubernetes/ |
| Go SDK / Docker 安装文件 | /golang/ 与 /docker-ce/ |
| Go / npm / pip / Maven | /go/、/npm/、/pip/simple/、/maven/ |
| Docker Hub 默认 API | /v2/ |
| 按仓库选择镜像 API | /v2/dockerhub/、/v2/k8s/、/v2/ghcr/、/v2/quay/、/v2/aliyun/、/v2/oracle/ |

例如访问 /github/sxxpqp/opskit/releases/...，由 Go 访问 github.com/sxxpqp/opskit/releases/... 后返回内容。
上游 HTTPS 重定向在服务器端跟随，npm/pip 元数据中的已知上游绝对地址改写到代理入口。
镜像 Bearer 认证地址在同上游或 auth_hosts 白名单内才允许转发；/auth/路径自动生成，无需手填。
上游增加新的认证域名时，需明确加入对应 auth_hosts，不能盲目开放任意域名。
客户端必须能使用与 public_url 一致的入口。容器镜像的 token、manifest、blob 仍需逐一上线实测。

## 4. 生成 opskit 配置

```sh
./opskit-cloud render --config config.yaml \
  --input /path/to/opskit/config.yaml --output config.generated.yaml
opskit doctor --config config.generated.yaml --profile local --output json
```

保留组件版本、校验摘要、SSH、数据库和 S3 字段值，改写已知安装下载源与 mirrors。输出不保留注释。
containerd 使用 /v2/<仓库> 端点并设置 override_path=true，HTTPS 不跳过证书校验。
Docker daemon 的 registry-mirrors 仅覆盖 Docker Hub，其他源应使用 containerd 映射或额外的域名入口。
HTTP 入口需另行配置客户端运行时允许该 HTTP registry；工具不会自动关闭 TLS 校验。
Maven 默认只有 Central；Go sumdb、OS apt/yum 和未知第三方 URL 不包含在默认代理范围。
不会绕过私有镜像权限、上游访问限制和限流；云服务器必须能访问上游。

## 5. GitHub 自动构建与部署

Settings → Environments → production，配置：

| 类型 | 名称 | 内容 |
|---|---|---|
| Variable | CLOUD_HOST | 云服务器 IP/主机名 |
| Variable | CLOUD_USER | root 或能免密 sudo 的 SSH 用户 |
| Variable | CLOUD_PORT | SSH 端口，默认 22 |
| Variable | CLOUD_ARCH | amd64 或 arm64，默认 amd64 |
| Variable | PROXY_PUBLIC_URL | 客户端访问地址，如 http://你的IP:8080 |
| Variable | PROXY_ALLOW_CIDRS | 客户端公网 CIDR，多个用英文逗号分隔 |
| Secret | CLOUD_PASSWORD | SSH 密码 |
| Secret | CLOUD_KNOWN_HOSTS | 经可信渠道核对的 SSH known_hosts 记录 |

密码不放明文 Variables。SSH 校验主机指纹，不在 CI 中盲信 ssh-keyscan。
服务器只需要 Linux + systemd、bash、tar、curl，不需要 Docker、Go 或 Python。
安全组放行受控来源的 SSH 和 8080。服务安装到 /opt/opskit-cloud，systemd 以动态非特权用户运行。
PR 自动测试、race、vet 和构建；main 自动部署；v* tag 自动发布二进制和示例配置。
部署前校验配置，失败尝试恢复上一版程序和配置。首次失败保留日志，使用 journalctl -u opskit-cloud 排查。
CI 默认按 GitHub 参数重新生成配置并覆盖云端 config.yaml；不要只在服务器手改配置，否则下次部署会被覆盖。
更多上游修改当前通过 config.example.yaml 或 init --upstream 实现；CI 参数化监听端口和 TLS 尚未接入，默认固定 HTTP 8080。
实际服务器信息尚未配置，因此尚未完成线上部署及各上游的真实连通验收。

## 验证

```sh
go test ./...
go vet ./...
./opskit-cloud serve --config config.example.yaml --check
curl http://127.0.0.1:8080/healthz
```
