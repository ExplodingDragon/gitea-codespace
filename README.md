# codespace

Gitea Codespace 的 Manager 与 Gateway 实现目录。

该模块负责：

- Manager 注册、声明、批量领取 operation 和生命周期 worker。
- Runtime Instance 映射、本地状态恢复与 Endpoint manifest 路由。
- Gateway 的 Endpoint、WebSocket 和 SSH 接入。
- ManagerService 客户端、Gateway session、日志脱敏和本地诊断。

在 Gitea Codespace 集成仓库中，系统职责、状态机、RPC 字段、配置和验收行为以 `src/README.md` 及其子文档为准。实现命令、配置示例和运行说明在对应功能可执行后随代码补充，避免把临时脚手架描述为已确定接口。

常用验证入口：

```bash
make test
make test-e2e-auto
make test-e2e-required
make test-e2e-manager-container-required
make test-e2e-manager-vm-required
make test-e2e-incus-matrix-required
```

`make test` 运行不依赖真实 Incus 的 Go 测试和脚本语法检查。`make test-e2e-auto` 只在本机 Incus client 可用且 `incus info` 成功时运行真实 E2E，否则输出跳过原因；`*-required` 入口用于 CI 或部署验收，Incus 条件不满足时直接失败。Manager 的 container 和 VM 入口都验证真实 create、stop、resume、Runtime Metadata、final 与 Incus exec/file 后端；matrix 按 provisioner container、Manager container、provisioner VM、Manager VM 的顺序串行执行，每次只创建一个内存限制为 `1GiB` 的实例。这样普通开发不被宿主环境阻塞，明确要求真实后端验收时也能得到硬错误，同时不会因并行测试超过低资源宿主容量。

Manager 配置只使用 YAML。`codespace.yaml` 是本地低侵入示例，`examples/config.example.yaml` 展示推荐的 `node / gateway / runtime` 结构、Incus project 命名空间、显式环境 tag、image 来源、已有实例复制和自定义 profile 引用。配置语义以 `src/gitea-server.md` 的 Manager 本地配置章节为准。
