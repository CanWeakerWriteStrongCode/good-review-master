// Package im 是 IM 协议实现端的入站入口（OneBot 12 webhook）。
//
// 它只做三件事：校验共享 token、把 OneBot 12 事件解码成 onebot.Event、按路径里的
// 接收账号投递给消费方。消息是否属于白名单群、是否 @ 了机器人、要不要回大模型，
// 全部留给 bot/router——这里是**传输层**，不含业务判断。
//
// 本包不感知平台差异：平台来自标准字段 self.platform，账号来自路径 {account}。
package im

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"
)

const (
	// maxEventBytes 单条事件的读取上限。正常群消息远小于这个值；
	// 设上限是为了避免一个坏客户端把连接内存打满。
	maxEventBytes = 1 << 20

	tokenHeader = "Authorization"

	// OneBot 12 webhook 标准头：版本号与实现名。实现名只用于排障日志。
	versionHeader = "X-OneBot-Version"
	implHeader    = "X-Impl"

	onebotVersion = "12"
)

// accountPattern 与工作空间 ID 规则保持一致；账号是路径段与整个 ID 的一部分。
var accountPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// platformPattern 是 OneBot 12 对平台名的约束：下划线非法，所以 qq_official 不行。
var platformPattern = regexp.MustCompile(`^[a-z][-a-z0-9]*(\.[-a-z0-9]+)*$`)

// EventConsumer 消费一条已解码的入站事件。
//
// account 来自上报路径，表示「这条事件是哪个机器人实例收的」；accepted=false
// 表示 Go 不接管（账号不在允许名单或无法归属），调用方回 404，实现端据此
// 不 stop 自己的 Agent，交回平台侧兜底。
//
// AcceptEvent 必须**快速返回**：HTTP 处理器在它返回后才回 2xx。
// 真正的处理（缓存、大模型）由实现方丢进自己的队列，不能在这里同步做大模型调用。
type EventConsumer interface {
	AcceptEvent(ctx context.Context, account string, event onebot.Event) (accepted bool, err error)
}

// Server 接收 IM 实现端推送的 OneBot 12 事件。
type Server struct {
	addr     string
	token    string
	consumer EventConsumer
	http     *http.Server
}

// NewServer 构造入站服务。token 为空表示不校验（仅建议在回环地址上这么用）。
func NewServer(addr, token string, consumer EventConsumer) *Server {
	s := &Server{
		addr:     addr,
		token:    token,
		consumer: consumer,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /im/event/{account}", s.handleEvent)
	mux.HandleFunc("GET /im/health", s.handleHealth)
	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// Handler 暴露处理器给单测；生产路径走 Start。
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Start 绑定端口并阻塞服务，直到 Shutdown 被调用。
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("监听 IM 事件入口 %s 失败：%w", s.addr, err)
	}
	logutil.Info("IM 事件入口已启动", "addr", "http://"+listener.Addr().String()+"/im/event/{account}")

	err = s.http.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭入站服务。
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventUnauthorized).Inc()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	account := strings.TrimSpace(r.PathValue("account"))
	if !accountPattern.MatchString(account) {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventBadRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid account in path; expected [A-Za-z0-9_-]{1,64}",
		})
		return
	}
	if version := strings.TrimSpace(r.Header.Get(versionHeader)); version != "" && version != onebotVersion {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventBadRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unsupported OneBot version " + version + "; expected " + onebotVersion,
		})
		return
	}

	var event onebot.Event
	// 同时留一份原始请求体：日志要看到实现端实际推来的 JSON（含 Go 结构体没解析的字段），
	// 而不是重新序列化后的精简版本。
	var rawBody bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(http.MaxBytesReader(w, r.Body, maxEventBytes), &rawBody))
	if err := decoder.Decode(&event); err != nil {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventBadRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid event json: " + err.Error()})
		return
	}
	// 多读一个字节确认没有尾随内容：避免"只解析了第一段 JSON"这种半截请求被当成成功。
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventBadRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "trailing data after event"})
		return
	}

	if platform := event.Self.Platform; platform != "" && !platformPattern.MatchString(platform) {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventBadRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid self.platform " + platform + "; expected [a-z][-a-z0-9]*(.[-a-z0-9]+)*",
		})
		return
	}

	if reason := ignoreReason(event); reason != "" {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventIgnored).Inc()
		logutil.Debug("忽略 IM 事件", "reason", reason, "type", event.Type,
			"detail_type", event.DetailType, "group", event.GroupID, "msgID", event.MessageID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	event.Account = account
	accepted, err := s.consumer.AcceptEvent(r.Context(), account, event)
	if err != nil {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventQueueFull).Inc()
		logutil.Warn("IM 事件入队失败", "account", account, "group", event.GroupID,
			"msgID", event.MessageID, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if !accepted {
		// 404 = 推送失败：实现端不 stop 自己的 Agent，交回平台侧兜底。
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventIgnored).Inc()
		logutil.Debug("忽略 IM 事件", "reason", "Go 未接管（账号不在允许名单）",
			"account", account, "platform", event.Platform(), "group", event.GroupID, "msgID", event.MessageID)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not managed by Go"})
		return
	}

	telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventAccepted).Inc()
	// event 是实现端实际推来的原始 JSON，未经过 Go 结构体二次序列化。
	logutil.Info("收到 IM 事件", "account", account, "impl", r.Header.Get(implHeader),
		"platform", event.Platform(), "group", event.GroupID, "msgID", event.MessageID,
		"event", json.RawMessage(readableEventJSON(rawBody.Bytes())))
	w.WriteHeader(http.StatusNoContent)
}

// readableEventJSON 把事件原文里的 \uXXXX 转义还原为 UTF-8 后重新序列化。
//
// 实现端用 JSON 推送时可能出现 ASCII 转义，中文原样落到日志里基本没法读。
// 数字用 UseNumber 保留原始字面量，不经过 float64。
// 解析失败时退回原文：日志宁可不那么好看，也不能因为"美化"吞掉排障信息。
func readableEventJSON(raw []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return raw
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return normalized
}

// ignoreReason 返回该事件为何不进入业务链路；空串表示应当消费。
//
// 只消费群消息：其它 type / detail_type 是合法事件，ack 后丢弃即可。
// 群消息自身缺关键字段则按无效事件处理。
func ignoreReason(event onebot.Event) string {
	if event.Type == "" {
		return "missing type"
	}
	if event.Type != "message" {
		return "type=" + event.Type
	}
	if event.DetailType != "group" {
		return "detail_type=" + event.DetailType
	}
	if event.Self.Platform == "" {
		return "missing self.platform"
	}
	if event.GroupID == "" {
		return "missing group_id"
	}
	if event.UserID == "" {
		return "missing user_id"
	}
	return ""
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	const bearer = "Bearer "
	header := r.Header.Get(tokenHeader)
	if !strings.HasPrefix(header, bearer) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, bearer)), []byte(s.token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
