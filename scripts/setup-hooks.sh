#!/bin/sh
# 启用本仓库的 git 钩子：每个克隆后运行一次。
set -e
cd "$(dirname "$0")/.." || exit 1
git config core.hooksPath scripts/githooks
echo "hooks 已启用：core.hooksPath=scripts/githooks"
