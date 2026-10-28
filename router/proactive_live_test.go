//go:build live

package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// liveClient 包一层真实 llm.Client，记录裁判结论与生成次数（只在 live 测试里用）。
type liveClient struct {
	llm.Client
	mu       sync.Mutex
	judges   int
	gens     int
	verdicts []string
}

func (c *liveClient) ChatJSON(ctx context.Context, messages []llm.Message, schema json.RawMessage) (*llm.ChatResponse, error) {
	resp, err := c.Client.ChatJSON(ctx, messages, schema)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.judges++
	if err == nil && resp != nil {
		c.verdicts = append(c.verdicts, resp.Content)
	}
	return resp, err
}

func (c *liveClient) SingleChat(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	c.mu.Lock()
	c.gens++
	c.mu.Unlock()
	return c.Client.SingleChat(ctx, chatLog, systemPrompt)
}

func (c *liveClient) snapshot() (int, int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.judges, c.gens, append([]string(nil), c.verdicts...)
}

// TestLiveProactive 用真实大模型跑主动插话的判定与生成链路。
// 出站打到本地假端点，不会真的往群里发消息。
func TestLiveProactive(t *testing.T) {
	// 按源文件位置定位仓库根目录：cwd 在不同 go test 环境下不一定稳定
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	t.Chdir(t.TempDir())
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)

	cfg, err := config.Load(config.Sources{
		Config: filepath.Join(repoRoot, "config.yaml"),
		Secret: filepath.Join(repoRoot, "secret.yaml"),
	})
	if err != nil {
		t.Fatalf("加载真实配置失败：%v", err)
	}
	cfg.ProactiveEnabled = true
	cfg.ProactiveCooldown = 0
	cfg.ProactiveJudgeEvery = 0
	cfg.ProactiveMaxStreak = 5
	cfg.ProactiveJudgeTimeout = 60 * time.Second
	cfg.LLMTimeout = 120 * time.Second
	cfg.BotNickname = "好评大师"
	cfg.MaxCacheMsg = 100
	cfg.LLMSendCount = 10

	promptCfg, err := config.LoadPromptConfig(
		filepath.Join(repoRoot, "prompt_system.yaml"),
		filepath.Join(repoRoot, "prompt_custom.yaml"),
	)
	if err != nil {
		t.Fatalf("加载提示词失败：%v", err)
	}

	// 图片场景用环境变量提供本地路径（例如自备的新闻截图与表情包），
	// 没设置就跳过两个图片子用例。路径必须落在 llm.image_local_dirs 内才能被读取。
	newsImage := os.Getenv("PROACTIVE_LIVE_NEWS_IMAGE")
	stickerImage := os.Getenv("PROACTIVE_LIVE_STICKER_IMAGE")
	for _, path := range []string{newsImage, stickerImage} {
		if path != "" {
			cfg.LLMConfig.ImageLocalDirs = append(cfg.LLMConfig.ImageLocalDirs, filepath.Dir(path))
		}
	}

	scenarios := []struct {
		name       string
		msgs       []cache.Message
		image      string // 纯图片场景：本地图片路径（需在 ImageLocalDirs 下）
		needsImage bool   // 没提供图片时跳过该用例
		expectS    bool   // 只用于日志对照，不作硬断言（判定权在模型）
	}{
		{
			name: "群友提问",
			msgs: []cache.Message{
				{MessageID: "1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "兄弟们，我的 Go 程序一启动就 panic"},
				{MessageID: "2", GroupID: "g1", UserID: "u2", Nickname: "李四", Content: "报什么错"},
				{MessageID: "3", GroupID: "g1", UserID: "u3", Nickname: "王五", Content: "有没有人知道 runtime error: invalid memory address 怎么排查"},
			},
			expectS: true,
		},
		{
			name: "两人闲聊",
			msgs: []cache.Message{
				{MessageID: "1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "晚上吃啥"},
				{MessageID: "2", GroupID: "g1", UserID: "u2", Nickname: "李四", Content: "随便"},
				{MessageID: "3", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "那就火锅"},
			},
			expectS: false,
		},
		{
			name: "单人独白",
			msgs: []cache.Message{
				{MessageID: "1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "今天好累啊"},
			},
			expectS: false,
		},
		{
			name: "叫机器人昵称",
			msgs: []cache.Message{
				{MessageID: "1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "好评大师在吗"},
			},
			expectS: true,
		},
		{
			name: "新闻截图",
			msgs: []cache.Message{
				{MessageID: "1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "[图片]"},
			},
			image:      newsImage,
			needsImage: true,
			expectS:    true,
		},
		{
			name: "表情包",
			msgs: []cache.Message{
				{MessageID: "1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "[图片]"},
			},
			image:      stickerImage,
			needsImage: true,
			expectS:    false,
		},
	}

	// 图片判定要求图片在允许读取的本地目录内
	cfg.LLMConfig.ImageLocalDirs = []string{filepath.Join(repoRoot, "vibe_images")}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if sc.needsImage {
				if sc.image == "" {
					t.Skip("未设置 PROACTIVE_LIVE_NEWS_IMAGE / PROACTIVE_LIVE_STICKER_IMAGE，跳过图片用例")
				}
				if _, err := os.Stat(sc.image); err != nil {
					t.Skipf("图片不存在，跳过：%v", err)
				}
			}
			store := cache.NewStore()
			var mu sync.Mutex
			var sent []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Action string         `json:"action"`
					Params map[string]any `json:"params"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				sent = append(sent, body.Params)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{}}`))
			}))
			defer srv.Close()

			client := &liveClient{Client: llm.NewOpenAIAdapter(
				cfg.LLMConfig.APIKey, cfg.LLMConfig.APIBase, cfg.LLMConfig.ModelName,
				cfg.LLMConfig.Temperature, cfg.LLMConfig.TopP)}
			r := NewRouter(staticSnapshot(cfg), promptCfg, client,
				onebot.NewClient(srv.URL, nil, "").For("live"), nil, context.Background())
			r.SetStore(store)
			r.SetWorkspace("live")

			gc := store.GetGroupCache("g1", cfg.MaxCacheMsg)
			for _, msg := range sc.msgs {
				gc.Add(msg)
			}
			last := sc.msgs[len(sc.msgs)-1]
			segments := []onebot.Segment{onebot.TextSegment(last.Content)}
			if sc.image != "" {
				segments = append(segments, onebot.ImageSegment(sc.image))
			}
			event := onebot.Event{
				Type: "message", DetailType: "group", GroupID: "g1",
				UserID: last.UserID, MessageID: last.MessageID,
				Self:    onebot.Self{Platform: "qq", UserID: "bot-openid"},
				Message: segments,
			}
			r.TryProactive(context.Background(), event, "g1", last.Content)

			deadline := time.Now().Add(3 * time.Minute)
			for {
				r.proactive.mu.Lock()
				running := r.proactive.groups["g1"] != nil && r.proactive.groups["g1"].running
				r.proactive.mu.Unlock()
				judges, _, _ := client.snapshot()
				if !running && judges > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("等待判定超时（judges=%d running=%v）", judges, running)
				}
				time.Sleep(200 * time.Millisecond)
			}

			judges, gens, verdicts := client.snapshot()
			for _, v := range verdicts {
				t.Logf("裁判输出：%s", strings.TrimSpace(v))
			}
			mu.Lock()
			sentCount := len(sent)
			var segmentTypes []string
			var texts []string
			if sentCount > 0 {
				if segs, ok := sent[0]["message"].([]any); ok {
					for _, seg := range segs {
						if m, ok := seg.(map[string]any); ok {
							segmentTypes = append(segmentTypes, m["type"].(string))
							if m["type"] == "text" {
								if data, ok := m["data"].(map[string]any); ok {
									texts = append(texts, data["text"].(string))
								}
							}
						}
					}
				}
			}
			mu.Unlock()
			t.Logf("期望回复=%v 实际 judges=%d gens=%d 出站消息=%d 段落=%v 文本=%q",
				sc.expectS, judges, gens, sentCount, segmentTypes, texts)

			if judges == 0 {
				t.Fatal("裁判没有被调用")
			}
			if sentCount > 0 {
				if len(segmentTypes) == 0 || segmentTypes[0] != "reply" {
					t.Errorf("主动回复应引用触发消息，实际段落 %v", segmentTypes)
				}
				for _, typ := range segmentTypes {
					if typ == "mention" {
						t.Errorf("主动回复不应 @ 人，实际段落 %v", segmentTypes)
					}
				}
			}
		})
	}
}
