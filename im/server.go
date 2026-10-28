// Package im 是 AstrBot sidecar 的入站入口。
//
// 它只做三件事：校验共享 token、把 OneBot 11 事件解码成 onebot.Event、投递给
// 消费方。消息是否属于白名单群、是否 @ 了机器人、要不要回大模型，全部留给 bot/router——
// 这里是**传输层**，不含业务判断。
//
// 出站方向不经过本包：发送仍然走 onebot.Client，由 AstrBot 插件的动作代理透传给平台。
package im

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

	// tokenHeader 与 OneBot HTTP API 的约定一致，复用同一个 access token。
	tokenHeader = "Authorization"
)

// EventConsumer 消费一条已解码的入站事件。
//
// AcceptEvent 必须**快速返回**：HTTP 处理器在它返回后才回 202。
// 真正的处理（缓存、大模型）由实现方丢进自己的队列，不能在这里同步做大模型调用。
type EventConsumer interface {
	AcceptEvent(ctx context.Context, event onebot.Event) error
}

// Server 接收 AstrBot 桥接插件推送的事件。
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
	mux.HandleFunc("POST /im/event", s.handleEvent)
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
	logutil.Info("IM 事件入口已启动", "addr", "http://"+listener.Addr().String()+"/im/event")

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

	var event onebot.Event
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventBytes))
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

	if reason := ignoreReason(event); reason != "" {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventIgnored).Inc()
		logutil.Debug("忽略 IM 事件", "reason", reason, "group", event.GroupID, "msgID", event.MessageID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": reason})
		return
	}

	if err := s.consumer.AcceptEvent(r.Context(), event); err != nil {
		telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventQueueFull).Inc()
		logutil.Warn("IM 事件入队失败", "group", event.GroupID, "msgID", event.MessageID, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	telemetry.IMEventsTotal.WithLabelValues(telemetry.IMEventAccepted).Inc()
	logutil.Info("收到 IM 事件", "platform", event.Platform, "group", event.GroupID,
		"msgID", event.MessageID, "user", event.Nickname)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// ignoreReason 返回该事件为何不进入业务链路；空串表示应当消费。
//
// 只接受群消息：本项目是群聊锐评机器人，私聊/通知事件即使推过来也没有对应处理路径，
// 在这里明确丢弃比让下游到处判空更清楚。
func ignoreReason(event onebot.Event) string {
	if event.PostType != "" && event.PostType != "message" {
		return "post_type=" + event.PostType
	}
	if event.MessageType != "" && event.MessageType != "group" {
		return "message_type=" + event.MessageType
	}
	if event.GroupID == "" {
		return "missing group_id"
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
