#!/bin/bash
# 只在依赖图**真的变了**的时候重跑 wire，没变就跳过。
#
# 为什么要这个门禁：`go tool wire ./app` 约 2.3 秒，而 `go build ./app` 只要 0.55 秒，
# 每次启动都跑是一笔真实开销。用 wire 自己的 `diff` 当门禁反而更差——diff 和 gen 一样贵
# （都是 ~2.3 秒），等于"检查 2.3 秒 + 生成 2.3 秒"，比不设门禁还慢。
#
# 门禁看的是**定义依赖图的那两个文件**（wire.go 的 provider 清单、providers.go 里的签名）
# 加上 go.mod（升一个依赖可能改变某个构造函数的形状），以 wire_gen.go 的时间戳为基准。
#
# 已知缺口，以及为什么可以接受：如果图里用到的构造函数在**别的包**改了签名
# （例如 mcpclient.New 多了一个参数），上面这几个文件的 mtime 都不变，门禁不会开——
# 但那时 wire_gen.go 里留着的是一个编译不过的调用，`go build` 会**直接报错**。
# 也就是说：这个门禁拿完备性换了速度，但没有拿正确性换——它不可能让你静默跑在旧图上。
#
# 门禁与 wire 都抓不到的唯一一种情况：新写了一个 provideXxx 却忘了加进 wire.Build。
# 那种情况生成物没有任何差异，唯一的发现方式是生成后看一眼 `git status`。
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

GENERATED="app/wire_gen.go"
GRAPH_INPUTS="app/wire.go app/providers.go go.mod"

if [ ! -f "$GENERATED" ]; then
    echo "[wire] app/wire_gen.go 不存在，生成中..."
elif [ -n "$(find $GRAPH_INPUTS -newer "$GENERATED" 2>/dev/null)" ]; then
    echo "[wire] 依赖图有更新，重新生成 app/wire_gen.go..."
else
    echo "[wire] 依赖图未变，跳过（省去一次约 2.3 秒的生成）"
    exit 0
fi

go tool wire ./app
echo "[wire] app/wire_gen.go 已更新"
