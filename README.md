# 呆呆

呆呆是一个使用 Go 编写的聊天机器人，支持 QQ 和 Telegram，并通过工具调用提供对话、记忆与系统操作能力。

## 功能

- QQ 群聊与私聊，支持按会话和用户隔离对话记忆
- Telegram 搭档私聊、群聊与有限插话，支持图片与文件、原生命令菜单和多层引用
- 自然语言调用工具，支持网页检索、系统状态查询和 AGY 工程任务
- 生成图文卡片，并处理用户明确要求分析的图片或文件
- 可选的 OCI 资源巡检与日报
- 群内可选碎碎念与梦境联想，按群独立开关，复用有上下文的有限插话
- 每日极客技术早报：四源精选、整期长图、原文索引和自然语言订阅，支持 QQ/TG 群聊与搭档私聊，每栏最多五条

## 快速开始

项目使用 Nix Flakes 提供开发环境。先复制示例配置，并填入 QQ 和模型服务所需的凭据：

```sh
cp config.example.yaml config.yaml
```

进入开发环境并启动：

```sh
nix develop
go run ./cmd/daidai -config config.yaml
```

`config.yaml` 含有密钥，请勿提交到版本库。

## 构建

使用 Nix 构建可执行程序：

```sh
nix build
```

构建结果位于 `result/bin/daidai`。也可以在开发环境中使用 Go 编译：

```sh
go build -o bin/daidai ./cmd/daidai
```

## NixOS 部署

仓库提供 NixOS 模块 `nix/module.nix`，Flake 输出为 `nixosModules.default`。启用 `services.daidai`，并将 `configFile` 指向服务运行时可读取的配置文件。含密钥的配置建议放在 `/run/secrets` 等运行时秘密目录中，避免进入 Nix Store。

## 文档

- [工作流总览](docs/workflow.md)
- [认知与消息处理](docs/workflow/cognition.md)
- [工单与执行器](docs/workflow/agents.md)
- [运行环境与部署](docs/workflow/runtime.md)
- [安全与运维](docs/workflow/security-operations.md)
- [极客早报与订阅](docs/workflow/briefing.md)
