# 呆呆

呆呆是一个使用 Go 编写的 QQ 机器人，支持群聊与私聊、按会话隔离的对话记忆，以及系统查询、网页检索和图片卡片等能力。

## 开发

需要安装 Nix 并启用 Flakes。

```sh
nix develop
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入所需的平台和模型配置
go run ./cmd/daidai -config config.yaml
```

构建可执行程序：

```sh
nix build
```

NixOS 模块位于 `nix/module.nix`。`services.daidai.configFile` 应指向运行时可读取的配置文件；不要将密钥提交到仓库。
