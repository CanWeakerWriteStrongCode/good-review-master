package cache

import (
	"fmt"
	"strings"
	"sync"

	"good-review-master/config"
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

// GroupMsgCache 群消息环形缓存
type GroupMsgCache struct {
	messages []Message
	mu       sync.RWMutex
}

var (
	cacheMap = make(map[string]*GroupMsgCache)
	cacheMu  sync.RWMutex
)

// GetGroupCache 获取或创建群消息缓存
func GetGroupCache(groupID string) *GroupMsgCache {
	cacheMu.RLock()
	gc, ok := cacheMap[groupID]
	cacheMu.RUnlock()

	if !ok {
		cacheMu.Lock()
		if gc, ok = cacheMap[groupID]; !ok {
			gc = &GroupMsgCache{messages: make([]Message, 0, config.MaxCacheMsg)}
			cacheMap[groupID] = gc
		}
		cacheMu.Unlock()
	}
	return gc
}

// Add 添加消息到缓存（环形队列，超出容量自动淘汰最早的）
func (gc *GroupMsgCache) Add(msg Message) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	if len(gc.messages) >= config.MaxCacheMsg {
		gc.messages = gc.messages[1:]
	}
	gc.messages = append(gc.messages, msg)
}

// GetAll 获取所有缓存消息（快照副本）
func (gc *GroupMsgCache) GetAll() []Message {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	msgs := make([]Message, len(gc.messages))
	copy(msgs, gc.messages)
	return msgs
}

// HasMsgID 检查消息ID是否已在缓存中
func (gc *GroupMsgCache) HasMsgID(msgID int64) bool {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	for i := len(gc.messages) - 1; i >= 0; i-- {
		if gc.messages[i].MsgID == msgID {
			return true
		}
	}
	return false
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
