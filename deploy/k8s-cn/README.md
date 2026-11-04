# dantoken.cn 国内部署

国内配置与 `deploy/k8s` / `deploy-aws.yml` 的海外环境独立。

- 集群：`aiworker-cn`，namespace：`new-api-cn`
- 数据库：`pgsql.aiworker.svc.cluster.local:5432/new_api_cn`，专用用户 `new_api_cn`
- 镜像：`registry.cn-hangzhou.aliyuncs.com/thecybersailor/new-api:cn-<git SHA>`
- GitHub Environment：`production-cn`
- DNS：`dantoken.cn` 指向控制集群公网 ALB

## 首次初始化

1. 通过控制集群 `aiworker/pgsql` 的本地端口转发建立独立数据库和账号。不要复用海外 DSN；应用账号应为 `new_api_cn` 库的 owner。RDS 模板的 `public` schema 可能仍属于管理员；连接到新库后执行 `ALTER SCHEMA public OWNER TO new_api_cn; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE, CREATE ON SCHEMA public TO new_api_cn;`，并验证该账号具备建表权限。
2. 创建 namespace；在其中创建 `new-api` Secret，包含 `SQL_DSN`、独立随机 `SESSION_SECRET` 和 `CRYPTO_SECRET`。SQL_DSN 必须指向上述国内库；键缺失将阻止 Pod 启动，避免回退到 SQLite。
3. 创建 ACR 拉取 Secret `acr-pull` 和域名证书 Secret `tls-dantoken-cn`。
4. 管理员执行 `kubectl --context aiworker-cn apply -k deploy/k8s-cn`，以及 `kubectl --context aiworker-cn apply -f deploy/k8s-cn/deployer-rbac.yaml`。bootstrap 镜像是占位符，首次 Action 部署前应用不会就绪。
5. 在 `production-cn` 中设置 `KUBE_CONFIG_CN`（原始 kubeconfig，限定上述部署权限）、`ALIYUN_ACR_USERNAME` 和 `ALIYUN_ACR_PASSWORD`。将环境部署分支限定为 `main`，定期轮换凭据。
6. 配置 DNS 后运行 `Deploy to China (dantoken.cn)`。main 更新也会自动触发。发布只构建和更新镜像；数据库、Secret 和 manifest 变更由管理员单独应用。

验收必须检查 rollout、HTTPS 证书，以及 `/api/status` 的 `success` 和 `data.version` 与本次 Git SHA 一致。新库首次启动后需完成站点管理员初始化。
