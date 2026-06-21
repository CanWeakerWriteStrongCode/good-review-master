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
	cache, ok := cacheMap[groupID]
	cacheMu.RUnlock()

	if !ok {
		cacheMu.Lock()
		if cache, ok = cacheMap[groupID]; !ok {
			cache = &GroupMsgCache{messages: make([]Message, 0, config.MaxCacheMsg)}
			cacheMap[groupID] = cache
		}
		cacheMu.Unlock()
	}
	return cache
}

// Add 添加消息到缓存（环形队列，超出容量自动淘汰最早的）
func (g *GroupMsgCache) Add(msg Message) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.messages) >= config.MaxCacheMsg {
		g.messages = g.messages[1:]
	}
	g.messages = append(g.messages, msg)
}

// GetAll 获取所有缓存消息（快照副本）
func (g *GroupMsgCache) GetAll() []Message {
	g.mu.RLock()
	defer g.mu.RUnlock()
	msgs := make([]Message, len(g.messages))
	copy(msgs, g.messages)
	return msgs
}

// HasMsgID 检查消息ID是否已在缓存中
func (g *GroupMsgCache) HasMsgID(msgID int64) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for i := len(g.messages) - 1; i >= 0; i-- {
		if g.messages[i].MsgID == msgID {
			return true
		}
	}
	return false
}

// BuildChatLog 将消息列表组装为群聊上下文文本
func BuildChatLog(msgs []Message) string {
	var sb strings.Builder
	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("%s：%s\n", msg.Nick, msg.Content))
	}
	return sb.String()
}
