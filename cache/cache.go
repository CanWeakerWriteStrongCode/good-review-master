package cache

import (
	"encoding/json"
	"strings"
	"sync"
)

// Message 群聊消息
//
// 字段命名对齐 OneBot 12 的 message_id / user_id / nickname；message_id 是字符串，
// 保持平台原值（QQ 官方是长数字串，Satori 可能是任意字符串）。
type Message struct {
	MessageID string   `json:"msg_id"`
	GroupID   string   `json:"group_id"`
	SelfID    string   `json:"self_id,omitempty"` // 收到这条消息的机器人账号；同一群多账号时用于去重
	UserID    string   `json:"user_id"`
	Nickname  string   `json:"nick"`
	Card      string   `json:"card"`
	Content   string   `json:"content"`
	Images    []string `json:"images,omitempty"` // 消息里的图片 URL（已 HTML 解码，供 agent「看图」使用）
	// ImageIDs 与 Images 一一对齐：图片内容标识（sha256 + 字节大小），持久化在 messages.image_ids。
	ImageIDs []ImageID `json:"image_ids,omitempty"`
	// ImageNotes 与 Images 一一对齐：全局图库里的描述，仅内存（重启时按 ImageIDs 回填）。
	ImageNotes []ImageNote `json:"-"`
	ReplyToID  string      `json:"quote_id,omitempty"` // 本条引用的消息 message_id；空=没有引用
	RefIdx     string      `json:"ref_idx,omitempty"`  // 平台侧引用索引（QQ 官方 REFIDX），用于反查被引消息
	Time       int64       `json:"time"`
}

// ImageID 图片内容标识。sha256 与字节大小联合使用：单看哈希有极小概率碰撞，
// 加大小一起比对成本可以忽略。
type ImageID struct {
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// ImageNote 全局图库（image_notes 表）里一张图的描述。
// 描述只写图本身、与聊天语境无关，所以可以跨群、跨账号复用。
// 注意：只存描述与尺寸，**不存文件地址**——旧消息的图可能已被清理，
// 候选列表展示的 url 始终取当前消息自己的 Images。
type ImageNote struct {
	ImageID
	MIME          string `json:"mime,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	Animated      bool   `json:"animated,omitempty"`
	Note          string `json:"note,omitempty"`
	InfoLevel     string `json:"info_level,omitempty"` // high|medium|low|none|unknown
	HasText       bool   `json:"has_text,omitempty"`
	Status        string `json:"status,omitempty"`         // ok|failed
	Attempts      int    `json:"attempts,omitempty"`       // 描述尝试次数（失败重试上限用）
	Model         string `json:"model,omitempty"`          // 生成描述用的模型名
	PromptVersion string `json:"prompt_version,omitempty"` // 描述提示词版本；换版不自动重刷
}

// GroupMsgCache 群消息环形缓存（零拷贝实现）
type GroupMsgCache struct {
	buf     []Message           // 固定大小环形缓冲区，只分配一次
	writeAt int                 // 下一个写入位置
	seenSet map[string]struct{} // O(1) 去重查找
	filled  bool                // 是否已写满一圈
	mu      sync.RWMutex
}

// LLMAnchor 缓存扩展窗口锚点（per-group）：记录上次发送窗口的首尾消息 message_id。
// Start 是窗口第一条消息（缓存命中前缀的起点），LastSent 是窗口最后一条消息（区分新增内容）。
type LLMAnchor struct {
	Start    string // 上次发送窗口首条消息 message_id
	LastSent string // 上次发送窗口末条消息 message_id
}

// Store 一个工作空间的消息缓存与选窗锚点。
//
// 多账号隔离的边界在这里：同一个物理群在两个账号下可能有相同 group_id
// （通用 OneBot），所以键里不能只有 group_id。每个工作空间持有自己的 Store，
// 包内只需要 group_id 一层键。
type Store struct {
	cacheMu     sync.RWMutex
	caches      map[string]*GroupMsgCache
	anchorMapMu sync.RWMutex
	anchors     map[string]*LLMAnchor
}

// NewStore 创建独立缓存存储（一个工作空间一个）。
func NewStore() *Store {
	return &Store{
		caches:  make(map[string]*GroupMsgCache),
		anchors: make(map[string]*LLMAnchor),
	}
}

var defaultStore = NewStore()

// DefaultStore 返回进程级默认 Store，仅供单账号测试、调试接口与离线评测使用。
// 生产链路必须使用工作空间自己的 Store。
func DefaultStore() *Store { return defaultStore }

// GetGroupCache 获取或创建群消息缓存
func (s *Store) GetGroupCache(groupID string, maxSize int) *GroupMsgCache {
	s.cacheMu.RLock()
	gc, ok := s.caches[groupID]
	s.cacheMu.RUnlock()

	if !ok {
		s.cacheMu.Lock()
		if gc, ok = s.caches[groupID]; !ok {
			gc = &GroupMsgCache{
				buf:     make([]Message, maxSize),
				seenSet: make(map[string]struct{}, maxSize),
			}
			s.caches[groupID] = gc
		}
		s.cacheMu.Unlock()
	}
	return gc
}

// Add 添加消息到环形缓存（满了直接覆盖，零拷贝）
//
// 这里刻意**没有**埋点。cache_messages 指标由消费侧（bot.ProcessMessage）按群更新：
// 在 Add 里写 prometheus 会给每条消息都增加分配，直接毁掉本函数的
// "零拷贝、0 allocs/op" 性质（cache/bench_test.go 把它钉住了）。
func (gc *GroupMsgCache) Add(msg Message) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	if msg.MessageID != "" {
		if _, ok := gc.seenSet[msg.MessageID]; ok {
			return
		}
	}

	// 淘汰即将被覆盖的老消息
	old := gc.buf[gc.writeAt]
	if old.MessageID != "" {
		delete(gc.seenSet, old.MessageID)
	}

	gc.buf[gc.writeAt] = msg
	if msg.MessageID != "" {
		gc.seenSet[msg.MessageID] = struct{}{}
	}
	gc.writeAt++
	if gc.writeAt >= len(gc.buf) {
		gc.writeAt = 0
		gc.filled = true
	}
}

// SetNickname 给某用户已缓存、但还没有昵称的消息补上显示名。
//
// 昵称往往在后一条消息里才第一次学到（QQ 官方事件通常不带 sender，桥接偶尔透传），
// 在此之前同一用户的消息都是空名；补上后拼给模型的聊天记录才能看出"谁说的"。
// 已有名字不改：历史名字可能与平台当前名不同，但那是当时真实显示的内容。
func (gc *GroupMsgCache) SetNickname(userID, name string) {
	if userID == "" || name == "" {
		return
	}
	gc.mu.Lock()
	defer gc.mu.Unlock()
	for i := range gc.buf {
		if gc.buf[i].UserID == userID && gc.buf[i].Nickname == "" {
			gc.buf[i].Nickname = name
		}
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

// CacheStats 群缓存的聚合状态（面板展示用）。
type CacheStats struct {
	Count    int    // 已缓存消息条数
	LastTime int64  // 最近一条消息的平台时间
	SelfID   string // 最近一条带账号信息的消息的 self_id；空表示无法归属
}

// Stats 一次加锁返回缓存的聚合状态。
//
// 面板只需要条数、最近活动时间和接收账号，没必要为每个群 GetAll 整份拷贝；
// 群缓存按 group_id 建，而 QQ 官方的 group_openid 按 bot 维度分配，
// 所以同一群的消息都来自同一个账号，取最近一条的 self_id 就够了。
func (gc *GroupMsgCache) Stats() CacheStats {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	n := gc.writeAt
	if gc.filled {
		n = len(gc.buf)
	}
	stats := CacheStats{Count: n}
	// 从最新写入位置往前找：最近时间与最近的非空 self_id 各取一次。
	for i := 0; i < n; i++ {
		idx := (gc.writeAt - 1 - i + len(gc.buf)) % len(gc.buf)
		msg := gc.buf[idx]
		if msg.MessageID == "" && msg.Content == "" && msg.Time == 0 {
			continue
		}
		if stats.LastTime == 0 {
			stats.LastTime = msg.Time
		}
		if stats.SelfID == "" {
			stats.SelfID = msg.SelfID
		}
		if stats.LastTime != 0 && stats.SelfID != "" {
			break
		}
	}
	return stats
}

// HasSeen 检查这条消息是否已经处理过（O(1) map 查找）。
// 一个 Store 只属于一个账号，所以 message_id 本身已经足够；selfID 参数保留
// 是为了调用方语义清晰，不参与判定。
func (gc *GroupMsgCache) HasSeen(selfID string, msgID string) bool {
	if msgID == "" {
		return false
	}
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	_, ok := gc.seenSet[msgID]
	return ok
}

// ListGroupIDs 返回所有已缓存的群号
func (s *Store) ListGroupIDs() []string {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	ids := make([]string, 0, len(s.caches))
	for id := range s.caches {
		ids = append(ids, id)
	}
	return ids
}

// GetCache 返回指定群的缓存引用，不存在返回 nil
func (s *Store) GetCache(groupID string) *GroupMsgCache {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.caches[groupID]
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

// chatLine 单条群聊消息在 JSON 行中的字段（发给大模型的表示）。
// UserID 是发言者的稳定标识（QQ 官方机器人是 openid，aiocqhttp 是数字 QQ），
// 昵称会变，它不会；为空时省略。
//
// MessageID/ReplyID 用的是本窗口内的序号，不是平台消息 ID：真实 ID 有上百个字符
// （QQ 官方 ROBOT1.0_…），模型既不输出也不寻址，白占 token。序号只在本次
// 聊天记录内有效；引用目标不在窗口里时 reply_id 省略。
type chatLine struct {
	MessageID int    `json:"message_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Nickname  string `json:"nickname"`
	Message   string `json:"message"`
	ReplyID   int    `json:"reply_id,omitempty"`
}

// BuildChatLog 将消息列表组装为发给大模型的群聊上下文：每条消息一行 JSON 对象，如
//
//	{"message_id":1,"user_id":"3319D767...","nickname":"张三","message":"今天好累"}
//	{"message_id":2,"user_id":"79EBED44...","nickname":"李四","message":"你也好","reply_id":1}
//
// message_id 是本窗口内的序号（从 1 开始，跳过空消息）；reply_id 指向窗口内被引用
// 消息的序号，被引消息不在窗口里时不输出。平台原始 message_id 仍保留在缓存/数据库/
// 锚点里，用于被动回复窗口与持久化，只有这份给大模型的文本用短序号。
// user_id 用字符串保留 openid / 数字 QQ 原值：昵称会变，稳定 ID 不会。
// 用 json.Marshal 保证昵称/内容里的引号、换行等被正确转义。空内容消息跳过。
func BuildChatLog(msgs []Message) string {
	// 先给会输出的行编号，reply_id 才能反查到窗口内序号。
	seq := make(map[string]int, len(msgs))
	next := 0
	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		next++
		if msg.MessageID != "" {
			seq[msg.MessageID] = next
		}
	}

	var buf strings.Builder
	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		replyID := 0
		if msg.ReplyToID != "" {
			replyID = seq[msg.ReplyToID]
		}
		line, err := json.Marshal(chatLine{
			MessageID: seq[msg.MessageID],
			UserID:    msg.UserID,
			Nickname:  msg.Nickname,
			Message:   msg.Content,
			ReplyID:   replyID,
		})
		if err != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.String()
}

// GetLLMAnchor 获取群的缓存扩展锚点
func (s *Store) GetLLMAnchor(groupID string) *LLMAnchor {
	s.anchorMapMu.RLock()
	defer s.anchorMapMu.RUnlock()
	return s.anchors[groupID]
}

// SetLLMAnchor 设置群的缓存扩展锚点
func (s *Store) SetLLMAnchor(groupID string, anchor LLMAnchor) {
	s.anchorMapMu.Lock()
	defer s.anchorMapMu.Unlock()
	a := anchor
	s.anchors[groupID] = &a
}

// FindMsgIndex 在消息列表中查找指定 message_id 的索引，未找到返回 -1
func FindMsgIndex(msgs []Message, msgID string) int {
	for i, msg := range msgs {
		if msg.MessageID == msgID {
			return i
		}
	}
	return -1
}

// ListAnchors 返回所有群的锚点副本（调试/测试用）
func (s *Store) ListAnchors() map[string]LLMAnchor {
	s.anchorMapMu.RLock()
	defer s.anchorMapMu.RUnlock()
	out := make(map[string]LLMAnchor, len(s.anchors))
	for k, v := range s.anchors {
		out[k] = *v
	}
	return out
}

// ResetAll 清空所有群缓存与锚点（仅测试/调试用）
func (s *Store) ResetAll() {
	s.cacheMu.Lock()
	s.caches = make(map[string]*GroupMsgCache)
	s.cacheMu.Unlock()

	s.anchorMapMu.Lock()
	s.anchors = make(map[string]*LLMAnchor)
	s.anchorMapMu.Unlock()
}

// 以下是默认 Store 的包级快捷方式，保留给测试、离线评测与 web 调试接口。
// 生产链路（bot/router）必须使用工作空间自己的 Store，否则多账号会串台。

func GetGroupCache(groupID string, maxSize int) *GroupMsgCache {
	return defaultStore.GetGroupCache(groupID, maxSize)
}

func GetCache(groupID string) *GroupMsgCache { return defaultStore.GetCache(groupID) }

func ListGroupIDs() []string { return defaultStore.ListGroupIDs() }

func GetLLMAnchor(groupID string) *LLMAnchor { return defaultStore.GetLLMAnchor(groupID) }

func SetLLMAnchor(groupID string, anchor LLMAnchor) { defaultStore.SetLLMAnchor(groupID, anchor) }

func ListAnchors() map[string]LLMAnchor { return defaultStore.ListAnchors() }

func ResetAll() { defaultStore.ResetAll() }
