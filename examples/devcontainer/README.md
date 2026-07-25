# devcontainer 自定义脚本案例

这个目录提供一套完整的 `init`、`start`、`stop` 自定义脚本，用来说明 Codespace Manager 如何通过通用脚本契约接入 devcontainer。设计重点是：Manager 管理 Incus 实例、通用输入、共享环境、结果文件、workspace 和实际 Endpoint 端口；Docker、devcontainer CLI、容器身份和容器内服务启动都由脚本负责。Gateway 的 shell、exec、SFTP 和 Web 终端由 Manager 通过 Incus API 进入实例 workspace，不要求 devcontainer 内运行 sshd。

示例配置：

```yaml
scripts:
  init: /path/to/codespace/examples/devcontainer/init.sh
  start: /path/to/codespace/examples/devcontainer/start.sh
  stop: /path/to/codespace/examples/devcontainer/stop.sh
```

部署要求：

- Incus 实例需要允许在实例内运行 Docker。常见方式是在测试或专用 project/profile 中开启 nesting，并为单实例设置不超过 `1GiB` 的内存限制。
- 实例镜像需要能使用 `apt-get`、`dnf` 或 `pacman` 安装基础工具。示例会安装 Docker、Node.js/npm、Git、OpenSSH client、sudo 和 Python。
- devcontainer CLI 由 `init.sh` 通过 npm 安装，默认版本为 `0.75.0`，可用环境变量 `DEVCONTAINER_EXAMPLE_CLI_VERSION` 覆盖。
- 如果 devcontainer 内的服务需要从浏览器访问，脚本或用户进程先把服务暴露到 Incus 实例内的实际端口，再调用 Runtime Endpoint API 声明该端口。

实现说明：

- `init.sh` 在 create 时克隆或复用已经完成的 workspace，按 create payload 锁定 commit，并安装 Docker、devcontainer CLI 和通用 helper。
- `start.sh` 是统一启动入口，create 后首次启动和 stopped 后恢复都执行它；它只使用已有 workspace，恢复 Git 凭据并启动或创建 devcontainer。
- `stop.sh` 在 Incus 实例停止前尽量停止已有 devcontainer，并把容器 ID 继续写入共享环境，供后续 start 复用。
- 普通 Endpoint 由容器或用户进程先暴露到 Incus 实例内的实际端口，再调用 Runtime Endpoint API。Manager 不读取容器端口或容器 ID。

实现验收点：

- [x] 三个脚本可以作为完整自定义套件显式配置，Manager 使用与内置脚本相同的 init、start、stop、共享环境和结果文件契约调用。
- [x] 示例把容器 ID 保存为 `DEVCONTAINER_EXAMPLE_CONTAINER_ID` 私有共享变量，Manager 只保存和传递该值，不解释其含义。
- [x] 示例通过 `CODESPACE_WORKSPACE_DIR` 提交通用 workspace 输出，并通过 `DEVCONTAINER_EXAMPLE_CONTAINER_ID` 保存脚本私有恢复信息。
- [x] start 不执行 checkout，不依赖 repository payload，恢复已有 workspace、Git 凭据和 devcontainer。
