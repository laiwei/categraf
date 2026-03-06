#!/usr/bin/env bash
set -euo pipefail

# 用法：
#   scripts/split_servicemap_aggregator.sh /abs/path/to/new-repo [git@github.com:org/servicemap-aggregator.git]
#
# 说明：
# - 从当前仓库的 servicemap-aggregator 子目录提取历史为独立仓库。
# - 如果提供 remote URL，会自动 push 到 main 分支。

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "Usage: $0 /abs/path/to/new-repo [remote-url]" >&2
  exit 1
fi

TARGET_REPO="$1"
REMOTE_URL="${2:-}"
SPLIT_BRANCH="split/servicemap-aggregator"

ROOT_DIR="$(git rev-parse --show-toplevel)"
cd "$ROOT_DIR"

if [[ ! -d "servicemap-aggregator" ]]; then
  echo "E: servicemap-aggregator directory not found at repo root" >&2
  exit 1
fi

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "E: working tree is not clean. Please commit/stash changes first." >&2
  exit 1
fi

if ! git ls-tree --name-only HEAD | grep -q '^servicemap-aggregator$'; then
  echo "E: servicemap-aggregator is not in HEAD. Commit the directory move first." >&2
  exit 1
fi

# 提取子目录历史
if git show-ref --verify --quiet "refs/heads/${SPLIT_BRANCH}"; then
  git branch -D "${SPLIT_BRANCH}" >/dev/null
fi

git subtree split --prefix=servicemap-aggregator -b "${SPLIT_BRANCH}" >/dev/null

# 初始化目标仓库
mkdir -p "$TARGET_REPO"
cd "$TARGET_REPO"

if [[ ! -d .git ]]; then
  git init -b main >/dev/null
fi

# 清空旧内容（保留 .git）
find . -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +

# 导入 split 分支内容
cd "$ROOT_DIR"

git archive "${SPLIT_BRANCH}" | tar -x -C "$TARGET_REPO"

cd "$TARGET_REPO"

git add .
if git diff --cached --quiet; then
  echo "I: no content changes to commit"
else
  git commit -m "chore: split servicemap-aggregator from monorepo" >/dev/null || true
fi

if [[ -n "$REMOTE_URL" ]]; then
  if git remote get-url origin >/dev/null 2>&1; then
    git remote set-url origin "$REMOTE_URL"
  else
    git remote add origin "$REMOTE_URL"
  fi
  git push -u origin main
fi

cd "$ROOT_DIR"
git branch -D "${SPLIT_BRANCH}" >/dev/null || true

echo "Done: standalone repo prepared at $TARGET_REPO"
