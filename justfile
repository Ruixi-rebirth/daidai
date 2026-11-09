default:
    @just --list

# 进入 Nix 开发环境
dev:
    nix --extra-experimental-features "nix-command flakes" develop

# 编译项目
build:
    nix --extra-experimental-features "nix-command flakes" develop --command go build -v -o bin/daidai ./cmd/daidai

# 运行测试
test:
    nix --extra-experimental-features "nix-command flakes" develop --command go test -v ./...

# 代码格式化与质量检查
lint:
    nix --extra-experimental-features "nix-command flakes" develop --command golangci-lint run ./...

# 运行程序
run:
    nix --extra-experimental-features "nix-command flakes" develop --command go run ./cmd/daidai
