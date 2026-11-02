package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"good-review-master/logutil"

	"github.com/gin-gonic/gin"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

// 语言协商用到的两个常量。
const (
	// LangHeader 是前端显式选择的语言。
	//
	// 为什么不用标准的 Accept-Language：那个头浏览器会自动带上（值是浏览器/系统语言），
	// 用户在前端点成英文、浏览器是中文时，后端收到的仍是中文——一个头里分不出
	// "用户选的"和"浏览器默认的"。分开两个来源，优先级才说得清。
	LangHeader = "X-Lang"

	// DefaultLang 是兜底语言：请求没带语言、带了个不认识的语言、或者语言文件缺条目时都落到它。
	DefaultLang = "zh-CN"

	// localeDirName 是语言文件目录名，落在进程工作目录下（与 config.yaml 同级）。
	// 放外面而不嵌进二进制，是为了"改一句文案不用重新编译"。
	localeDirName = "locales"

	// localeWatchInterval 是语言目录的轮询间隔。改完文案最坏等这么久生效。
	// 与配置文件监听用同一个量级：每次 tick 只做几次 stat，开销可忽略。
	localeWatchInterval = 2 * time.Second
)

// 语言文件全坏时的最后一句。它必须写死在代码里——文案本身就在文件里，
// 文件读不出来时再去文件里找兜底就成了死循环。
const (
	unknownTextZH = "未知错误，请联系客服"
	unknownTextEN = "Unknown error. Please contact support."
)

// Locales 管着一份语言包，并支持在不重启进程的前提下整体替换。
//
// 为什么是"整体重建"而不是"逐条更新"：语言文件是被整体覆盖写的，
// 重新解析一遍比算差集简单得多，而且读侧永远不会看到"旧的删了、新的还没加"的中间态。
//
// 读侧无锁（atomic.Pointer），这是本项目所有热更新数据的统一做法（见 config.PromptConfig）：
// 发布点只有一个，调用方拿到的要么是完整的旧版本、要么是完整的新版本。
type Locales struct {
	dir string
	// bundle / langs 成对发布。分开两个指针会出现"bundle 换了但 langs 还是旧的"的瞬间，
	// 那一瞬间按旧列表里的语言去新 bundle 里查，会查不到。
	state atomic.Pointer[localeState]
}

type localeState struct {
	bundle *i18n.Bundle
	// langs 是已加载的语言（文件名去掉 .yaml），按字典序。
	langs []string
}

// NewLocales 从 dir 加载语言文件；文件缺失时先把内嵌的模板写出去再加载。
//
// 目录取的是外部路径而不是 go:embed 直接读：错误文案改一个字就要重新编一个
// 二进制太难受了，放外面才能"改文件、两秒后生效"（见 Reload）。
func NewLocales(dir string) *Locales {
	l := &Locales{dir: dir}
	ensureLocaleFiles(dir)
	l.Reload()
	return l
}

// Reload 重新读整个目录并原子替换。
//
// 失败时**保留上一版**而不是清空：语言文件写坏了一半的时候，
// 面板宁可继续显示旧文案，也不该突然满屏"未知错误"。
func (l *Locales) Reload() {
	state, err := loadLocales(l.dir)
	if err != nil {
		if l.state.Load() == nil {
			// 启动阶段就一份都没加载成功：后面每次查文案都会落到代码里的兜底句
			logutil.Error("语言文件加载失败，错误文案将使用内置兜底", "dir", l.dir, "err", err)
			return
		}
		logutil.Error("语言文件重新加载失败，继续使用上一版", "dir", l.dir, "err", err)
		return
	}
	l.state.Store(state)
	logutil.Info("语言文件已加载", "dir", l.dir, "语言", strings.Join(state.langs, ","))
}

// requestLang 决定这次响应用哪种语言。
//
// 优先级：X-Lang（用户显式选的）> Accept-Language（浏览器默认）> zh-CN。
func (l *Locales) requestLang(c *gin.Context) string {
	langs := l.loadedLangs()

	if lang := matchLang(c.GetHeader(LangHeader), langs); lang != "" {
		return lang
	}
	for _, candidate := range acceptLanguageTags(c.GetHeader("Accept-Language")) {
		if lang := matchLang(candidate, langs); lang != "" {
			return lang
		}
	}
	if lang := matchLang(DefaultLang, langs); lang != "" {
		return lang
	}
	// 连默认语言的文件都被删了：退到目录里排第一的那个，至少还有文案可查
	if len(langs) > 0 {
		return langs[0]
	}
	return DefaultLang
}

// text 把 message id 翻成一句话。
//
// 三级回落：请求语言 → 默认语言（漏写的几乎都是非默认那份）→ 代码里的兜底句。
// **绝不返回空串**：前端拿到空 msg 会退回本地兜底"未知错误"，
// 那句话会把真实原因盖掉，排查时看不到任何线索。
func (l *Locales) text(lang, id string, args map[string]any) string {
	state := l.state.Load()
	if state == nil {
		return unknownText(lang)
	}
	if msg := localizeOne(state.bundle, lang, id, args); msg != "" {
		return msg
	}
	if msg := localizeOne(state.bundle, DefaultLang, id, args); msg != "" {
		logutil.Warn("语言文件缺少该条目，已回落到默认语言", "id", id, "lang", lang)
		return msg
	}
	logutil.Warn("语言文件里找不到该条目，使用内置兜底句", "id", id, "lang", lang)
	return unknownText(lang)
}

// has 判断某个语言是否已加载，供测试核对"两份文件都在"。
func (l *Locales) has(lang string) bool {
	for _, loaded := range l.loadedLangs() {
		if loaded == lang {
			return true
		}
	}
	return false
}

func (l *Locales) loadedLangs() []string {
	if state := l.state.Load(); state != nil {
		return state.langs
	}
	return nil
}

// loadLocales 读目录里的全部 yaml，建成一份新状态。
func loadLocales(dir string) (*localeState, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取语言目录 %s 失败：%w", dir, err)
	}

	bundle := i18n.NewBundle(language.SimplifiedChinese)
	bundle.RegisterUnmarshalFunc("yaml", yaml.Unmarshal)

	var langs, broken []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(dir, name)
		// 单个文件坏掉不影响其他语言：记下来、继续加载。
		// 整个目录一份都不剩才算失败，那时才保留上一版。
		if _, err := bundle.LoadMessageFile(path); err != nil {
			logutil.Warn("语言文件解析失败，已跳过", "file", path, "err", err)
			broken = append(broken, name)
			continue
		}
		langs = append(langs, strings.TrimSuffix(name, ".yaml"))
	}
	if len(langs) == 0 {
		return nil, fmt.Errorf("目录 %s 里没有可用的语言文件（坏掉：%s）", dir, strings.Join(broken, ","))
	}

	sort.Strings(langs)
	return &localeState{bundle: bundle, langs: langs}, nil
}

// localizeOne 查一条文案，查不到返回空串。
func localizeOne(bundle *i18n.Bundle, tag, id string, args map[string]any) string {
	msg, err := i18n.NewLocalizer(bundle, tag).Localize(&i18n.LocalizeConfig{
		MessageID:    id,
		TemplateData: args,
	})
	if err != nil {
		return ""
	}
	return msg
}

// matchLang 在已加载的语言里找一个与 candidate 对得上的。
//
// 允许主语言与地区语言互认（en ↔ en-US）：前端可能只传 en，文件却是 en-US.yaml。
// 找不到返回空串，让调用方决定下一步（而不是在这里就落默认语言——
// 那样 Accept-Language 里第二个候选就没机会了）。
func matchLang(candidate string, langs []string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	lower := strings.ToLower(candidate)
	for _, lang := range langs {
		if strings.ToLower(lang) == lower {
			return lang
		}
	}
	for _, lang := range langs {
		loaded := strings.ToLower(lang)
		if strings.HasPrefix(loaded, lower+"-") || strings.HasPrefix(lower, loaded+"-") {
			return lang
		}
	}
	return ""
}

// acceptLanguageTags 把 Accept-Language 拆成候选列表。
//
// 不按 q 值重排：浏览器发出来的这个头本来就按优先级降序排好了，
// 自己再排一遍只是在猜规则。这里只负责拆出 tag，顺序原样保留。
func acceptLanguageTags(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	tags := make([]string, 0, len(parts))
	for _, part := range parts {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if tag == "" || tag == "*" {
			continue
		}
		tags = append(tags, tag)
	}
	return tags
}

// unknownText 返回兜底句。英文请求给英文，其余给中文。
func unknownText(lang string) string {
	if strings.HasPrefix(strings.ToLower(lang), "en") {
		return unknownTextEN
	}
	return unknownTextZH
}

// localePoller 是"语言目录变了"的信号源，实现 store.Source。
//
// 为什么不能直接用 store.FilePoller：那个盯的是一份固定的路径列表，
// 而这里**文件名本身也可能变**（新增一门语言 = 往目录里丢一个文件）。
// 只看目录的 mtime 也不够：改文件内容不会更新目录的修改时间。
// 所以指纹要同时包含文件列表和每个文件的 (大小, 修改时间)。
type localePoller struct {
	dir   string
	every time.Duration
}

func newLocalePoller(dir string, every time.Duration) *localePoller {
	if every <= 0 {
		every = 2 * time.Second
	}
	return &localePoller{dir: dir, every: every}
}

// Changed 每 every 检查一次指纹，变了就往 channel 里塞一个信号。
// 容量 1、非阻塞发送：信号表达的是"该重读了"，连变两次只需重读一次。
func (p *localePoller) Changed(ctx context.Context) <-chan struct{} {
	changed := make(chan struct{}, 1)
	go func() {
		defer close(changed)

		previous := p.fingerprint()
		ticker := time.NewTicker(p.every)
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
				default:
				}
			}
		}
	}()
	return changed
}

// fingerprint 把目录下每个 yaml 的 (文件名, 大小, 修改时间) 拼成一个串。
// os.ReadDir 返回按文件名排序，所以同样一批文件每次拼出来的结果一样。
func (p *localePoller) fingerprint() string {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return "missing"
	}
	var b strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		b.WriteString(entry.Name())
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(info.Size(), 10))
		b.WriteByte('|')
		b.WriteString(info.ModTime().String())
		b.WriteByte(';')
	}
	return b.String()
}
