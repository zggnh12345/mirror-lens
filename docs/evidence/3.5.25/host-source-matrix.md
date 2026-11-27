# Host 来源矩阵（G0，据 docs/3.5.8-registry-phase1-design.md 84b013d）

## 范围与默认准入（:6,:99）

8 组公开 Registry：gcr.io、registry.k8s.io、*.pkg.dev、quay.io、mcr.microsoft.com、registry.gitlab.com、public.ecr.aws、cgr.dev；lscr.io 当批不启用（:61,:101,:738,:792）。

## 匿名 /v2/ 挑战实测（:46-61）

| Host | /v2/ | WWW-Authenticate |
|---|---|---|
| gcr.io | 401 | Bearer realm=https://gcr.io/v2/token service=gcr.io（:50） |
| us-docker.pkg.dev | 401 | Bearer realm=https://us-docker.pkg.dev/v2/token（:51） |
| registry.k8s.io | 401 | Bearer realm=https://registry.k8s.io/token service=registry.k8s.io（:52） |
| quay.io | 401 | Bearer realm=https://quay.io/v2/auth service=quay.io（:53） |
| mcr.microsoft.com | 200 | 无 Challenge（:54） |
| lscr.io | 401 | Bearer realm=https://ghcr.io/token service=ghcr.io（:55） |

lscr.io 按真实挑战把 DefaultRealms 修正为 https://ghcr.io 而非按 Host 名推导（:61,:300,:792）。D9 Realm 覆盖（:290-302）：gitlab.com/jwt/auth host=gitlab.com(:295)、public.ecr.aws/token/(:296)、cgr.dev/token(:297)、mcr 匿名 200 无 Bearer 不构造 Token 流程(:298)、lscr.io 跨 Host 待验(:300)。

## 真实拉取映射（:727-738，逐行含 Index/Manifest、平台 Manifest、Config Blob 的 sha256 digest，均通过）

| Host | 代表仓库 |
|---|---|
| gcr.io | distroless/static:nonroot（:729） |
| registry.k8s.io | pause:3.9（:730） |
| us-docker.pkg.dev | google-samples/containers/gke/hello-app:1.0（:731） |
| quay.io | prometheus/busybox:latest（:732） |
| mcr.microsoft.com | hello-world:latest（:733） |
| registry.gitlab.com | gitlab-org/cli:latest（:734） |
| public.ecr.aws | docker/library/alpine:latest（:735） |
| cgr.dev | chainguard/static:latest（:736） |

public.ecr.aws 无 Docker-Content-Digest 头、用正文 SHA-256；lscr.io 不在本批未验收（:738）。3.5.25 任务书 :780-794 沿用上述 8 仓库（`*.pkg.dev` 对应 us-docker.pkg.dev 仓库），另加 lscr.io: linuxserver/traefik（:791）；证据分离规则：mcr 200 无 challenge_realms 记 N/A、configured_policy_realms 单列（:380），lscr.io primary_realm 必须为 https://ghcr.io（:382）。

## 未关闭取证项

- D2：Manifest 404/410 的真实语义仍待取证，任务书冻结为 `HOLD_CURRENT_404_SEMANTICS`，不得判 VerdictNo。
- lscr.io 真实 Host 能力（token challenge + manifest 验证）待 G5；当前只有 401 challenge 证据。
- SparkCR/SUNBALCONY 真实告警根因待 G0/G5 从证据调查，不得猜测。
