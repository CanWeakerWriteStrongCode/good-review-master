package cache

import (
	"fmt"
	"strings"
	"sync"
)

// Message 群聊消息
type Message struct {
	MsgID   int64
	GroupID string
	UserID  string
	Nick    string
	Content string
	Time    int64
}

// GroupMsgCache 群消息环形缓存（零拷贝实现）
type GroupMsgCache struct {
	buf      []Message          // 固定大小环形缓冲区，只分配一次
	writeAt  int                // 下一个写入位置
	msgIDSet map[int64]struct{} // O(1) 去重查找
	filled   bool               // 是否已写满一圈
	mu       sync.RWMutex
}

var (
	cacheMap = make(map[string]*GroupMsgCache)
	cacheMu  sync.RWMutex
)

// GetGroupCache 获取或创建群消息缓存
func GetGroupCache(groupID string, maxSize int) *GroupMsgCache {
	cacheMu.RLock()
	gc, ok := cacheMap[groupID]
	cacheMu.RUnlock()

	if !ok {
		cacheMu.Lock()
		if gc, ok = cacheMap[groupID]; !ok {
			gc = &GroupMsgCache{
				buf:      make([]Message, maxSize),
				msgIDSet: make(map[int64]struct{}, maxSize),
			}
			cacheMap[groupID] = gc
		}
		cacheMu.Unlock()
	}
	return gc
}

// Add 添加消息到环形缓存（满了直接覆盖，零拷贝）
func (gc *GroupMsgCache) Add(msg Message) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	// 淘汰即将被覆盖的老消息
	old := gc.buf[gc.writeAt]
	if old.MsgID != 0 {
		delete(gc.msgIDSet, old.MsgID)
	}

	gc.buf[gc.writeAt] = msg
	gc.msgIDSet[msg.MsgID] = struct{}{}
	gc.writeAt++
	if gc.writeAt >= len(gc.buf) {
		gc.writeAt = 0
		gc.filled = true
	}
}

// GetAll 获取所有缓存消息（按时间排序的快照副本）
func (gc *GroupMsgCache) GetAll() []Message {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	if !gc.filled {
		// 没满一圈，直接返回 [0, writeAt)
		msgs := make([]Message, gc.writeAt)
		copy(msgs, gc.buf[:gc.writeAt])
		return msgs
	}

	// 已满一圈，返回 [writeAt, end) + [0, writeAt) 保持时间顺序
	n := len(gc.buf)
	msgs := make([]Message, n)
	tailLen := copy(msgs, gc.buf[gc.writeAt:])
	copy(msgs[tailLen:], gc.buf[:gc.writeAt])
	return msgs
}

// HasMsgID 检查消息ID是否已在缓存中（O(1) map 查找）
func (gc *GroupMsgCache) HasMsgID(msgID int64) bool {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	_, ok := gc.msgIDSet[msgID]
	return ok
}

// ListGroupIDs 返回所有已缓存的群号
func ListGroupIDs() []string {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	ids := make([]string, 0, len(cacheMap))
	for id := range cacheMap {
		ids = append(ids, id)
	}
	return ids
}

// GetCache 返回指定群的缓存引用，不存在返回 nil
func GetCache(groupID string) *GroupMsgCache {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	return cacheMap[groupID]
}

// Len 返回缓存中的消息数量
func (gc *GroupMsgCache) Len() int {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	if gc.filled {
		return len(gc.buf)
	}
	return gc.writeAt
}

// BuildChatLog 将消息列表组装为群聊上下文文本
func BuildChatLog(msgs []Message) string {
	var buf strings.Builder
	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		buf.WriteString(fmt.Sprintf("%s：%s\n", msg.Nick, msg.Content))
	}
	return buf.String()
}
