package pool

import (
	"runtime"
	"sync"
)

// Pool 通用协程池，固定 worker 数量，有界任务队列
type Pool struct {
	tasks chan func()
	wg    sync.WaitGroup
	once  sync.Once
}

// New 创建协程池。size 为 worker 数量，<=0 时取 runtime.NumCPU()*2
func New(size int) *Pool {
	if size <= 0 {
		size = runtime.NumCPU() * 2
	}
	pool := &Pool{
		tasks: make(chan func(), size*2),
	}
	for i := 0; i < size; i++ {
		pool.wg.Add(1)
		go pool.worker()
	}
	return pool
}

// worker 从任务队列中消费并执行，channel 关闭后自动排空并退出
func (p *Pool) worker() {
	defer p.wg.Done()
	for task := range p.tasks {
		task()
	}
}

// Submit 提交任务，非阻塞。池已关闭或队列满时返回 false
func (p *Pool) Submit(task func()) bool {
	select {
	case p.tasks <- task:
		return true
	default:
		return false
	}
}

// Shutdown 优雅关闭：停止接收新任务，等待所有已提交任务执行完毕后返回
func (p *Pool) Shutdown() {
	p.once.Do(func() {
		close(p.tasks)
	})
	p.wg.Wait()
}
