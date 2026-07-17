# codespace

Gitea Codespace 的 Manager 与 Gateway 实现目录。

该模块负责：

- Manager 注册、声明、批量领取 operation 和生命周期 worker。
- Runtime Instance 映射、本地状态恢复与 Runtime HTTP API。
- Gateway 的 Endpoint、WebSocket 和 SSH 接入。
- ManagerService 客户端、Gateway session、日志脱敏和本地诊断。

在 Gitea Codespace 集成仓库中，系统职责、状态机、RPC 字段、配置和验收行为以 `src/README.md` 及其子文档为准。实现命令、配置示例和运行说明在对应功能可执行后随代码补充，避免把临时脚手架描述为已确定接口。
