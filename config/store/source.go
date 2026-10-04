package store

import (
	"context"
	"os"
	"strconv"
	"time"
)

// Source 提供"该重读了"的信号，对标 client-go 的 Watch 接口。
//
// 与真正的 Watch 有一处本质差别，必须写在最前面免得误读：
// **文件源没有增量。** 文件是被整体覆盖写的，没有"某个字段被改了"这种事件，
// 所以这里的信号只表达"变了"，每一次变化都等价于一次完整重新 List。
// 这也是为什么 Informer 收到信号后不做任何增量合并，而是整个换掉快照。
type Source interface {
	// Changed 返回一个 channel，每收到一个值代表"来源变了，请重读"。
	// ctx 取消后 channel 关闭。
	Changed(ctx context.Context) <-chan struct{}
}

// FilePoller 按固定间隔轮询若干文件的指纹（修改时间 + 大小），
// 任何一个与上次不同就发一个信号。
//
// 为什么是轮询而不是 fsnotify：本项目要盯的是几个 YAML，轮询零依赖、跨平台一致、
// 且天然容忍"文件被编辑器整体重写"（很多编辑器是写临时文件再 rename，
// 基于 inode 的监听会漏掉这类事件）。代价是最长 interval 的发现延迟。
type FilePoller struct {
	paths    []string
	interval time.Duration
}

// NewFilePoller 构造轮询源。interval <= 0 时取 2s。
func NewFilePoller(interval time.Duration, paths ...string) *FilePoller {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &FilePoller{paths: paths, interval: interval}
}

// Changed 实现 Source。第一个信号在 interval 之后才可能发出——
// 初始快照由调用方在构造 Store 时给，不需要这里再报一次"变了"。
func (p *FilePoller) Changed(ctx context.Context) <-chan struct{} {
	// 容量 1：信号是"现在该重读了"的**电平**而不是要计数的**边沿**，
	// 连变两次只需重读一次。缓冲 1 让发送方永不阻塞，
	// 消费不及时也不会漏掉"至少要重读一次"这件事。
	changed := make(chan struct{}, 1)

	go func() {
		defer close(changed)

		previous := p.fingerprint()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current := p.fingerprint()
				if current == previous {
					continue
				}
				previous = current
				select {
				case changed <- struct{}{}:
				default: // 已有未消费的信号，不必再塞
				}
			}
		}
	}()

	return changed
}

// fingerprint 把全部被盯文件的 (修改时间, 大小) 拼成一个字符串。
//
// 只看修改时间不够：同一秒内的两次写入在粗粒度文件系统上可能拿到相同时间戳，
// 加上大小能挡住"内容变了但长度恰好没变"之外的大部分情况。
// 文件不存在时记为空——文件被删掉/新建同样是一次值得重读的变化。
func (p *FilePoller) fingerprint() string {
	result := ""
	for _, path := range p.paths {
		info, err := os.Stat(path)
		if err != nil {
			result += path + "|missing;"
			continue
		}
		result += path + "|" + info.ModTime().String() + "|" +
			strconv.FormatInt(info.Size(), 10) + ";"
	}
	return result
}
