package server

import (
	"errors"
	"fmt"
	"net/http"

	"good-review-master/logutil"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

// BizError 是"业务上不允许"这一类错误，对应 Java 体系里的 BusinessException。
//
// 它只携带三样东西：码、模板参数、原始原因。**文案不在这里**——
// 文案由 ErrorHandler 按请求语言现查语言包，所以同一个 BizError
// 对中国用户和外国用户会渲染成两句话，而业务代码完全不用关心这一点。
type BizError struct {
	// Code 决定两件事：body 里的 code、以及配哪个 HTTP 状态码（见 errTable）。
	Code ErrCode
	// Args 是文案模板的参数，key 与 yaml 里的 {{.Key}} 对应。
	// 常用的只有 workspace.multiple 的 Endpoint，多数错误传 nil 即可。
	Args map[string]any
	// cause 是给日志看的原因，**永远不会出现在响应里**。
	// 有些业务错误是包着底层错误上来的（比如读库失败），原文只该进日志。
	cause error
}

// Error 让 BizError 满足 error 接口。日志里看到的是码的名字而不是数字，
// 这样搜日志时不用先回代码里查"60001 是什么"。
func (e *BizError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Code.messageID(), e.cause)
	}
	return e.Code.messageID()
}

// Unwrap 让 errors.As / errors.Is 能穿透到原始错误。
func (e *BizError) Unwrap() error { return e.cause }

// fail 在业务代码里声明一次失败：登记错误、终止后续 handler。
//
// 用法：
//
//	fail(c, CodeWorkspaceNotFound, nil)
//	return
//
// **不要在业务代码里直接写 c.JSON 返回错误**——那样就绕过了统一翻译，
// 中文会重新长回代码里，i18n 等于白做。
func fail(c *gin.Context, code ErrCode, args map[string]any) {
	_ = c.Error(&BizError{Code: code, Args: args})
	c.Abort()
}

// failWrap 与 fail 相同，但把底层错误挂上去供日志查看（响应里不会出现它）。
func failWrap(c *gin.Context, code ErrCode, cause error, args map[string]any) {
	_ = c.Error(&BizError{Code: code, Args: args, cause: cause})
	c.Abort()
}

// ErrorHandler 是面板 API 的统一错误出口，对应 Java 的 @ControllerAdvice。
//
// 两种结果，按错误类型分：
//
//	BizError   → 放开，用它的码和语言包文案回应（业务失败通常还是 HTTP 200）
//	其他 error → 统一压成"未知错误 + 单号"，原文只进日志（5xx）
//
// 中间件必须挂在 /api 组的**最外层**（在鉴权之前）：鉴权失败也是通过
// c.Error 上报的，挂晚了就接不住。
func ErrorHandler(locales *Locales) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if len(c.Errors) == 0 {
			return
		}
		if c.Writer.Written() {
			// 响应已经发出去了：要么是正常返回后才补记的错误（如异步收尾失败），
			// 要么是别的中间件抢先写了。此时再写一遍会把 body 拼成两段 JSON，
			// 前端解析必炸——所以只留痕。
			logutil.Warn("响应已写出，忽略后续错误",
				"path", c.Request.URL.Path, "err", c.Errors.Last().Err)
			return
		}
		writeBizError(c, locales, c.Errors.Last().Err)
	}
}

// writeBizError 把错误渲染成响应。
func writeBizError(c *gin.Context, locales *Locales, err error) {
	lang := locales.requestLang(c)

	var biz *BizError
	if errors.As(err, &biz) {
		entry := biz.Code.entry()
		if biz.cause != nil {
			// 业务错误本身不必记日志（密码错、资源不存在是正常现象，记了就是刷屏），
			// 但带着底层原因的要说一声：那通常意味着环境出了问题，光看响应看不出来。
			logutil.Warn("业务请求失败", "code", entry.ID,
				"path", c.Request.URL.Path, "err", biz.cause)
		}
		c.JSON(entry.HTTP, APIResponse{
			Code: int(biz.Code),
			Err:  entry.ID,
			Msg:  locales.text(lang, entry.ID, biz.Args),
		})
		return
	}

	// 意料之外的错误：原文只进日志，响应里只给一句兜底 + 单号。
	// 单号是给用户报障用的——他念出这一串，就能在日志里定位到唯一的现场。
	reqID := traceID(c)
	logutil.Error("请求处理失败",
		"method", c.Request.Method, "path", c.Request.URL.Path,
		"req_id", reqID, "err", err)
	c.JSON(http.StatusInternalServerError, APIResponse{
		Code:  int(CodeInternal),
		Err:   CodeInternal.messageID(),
		Msg:   locales.text(lang, CodeInternal.messageID(), nil),
		ReqID: reqID,
	})
}

// traceID 取当前请求的 trace id；没开链路追踪时返回空串。
func traceID(c *gin.Context) string {
	spanCtx := trace.SpanContextFromContext(c.Request.Context())
	if spanCtx.HasTraceID() {
		return spanCtx.TraceID().String()
	}
	return ""
}

// bizErrorOf 从 gin 的错误列表里取出业务错误，供埋点判断"这条请求业务上成没成"。
func bizErrorOf(c *gin.Context) (*BizError, bool) {
	for _, item := range c.Errors {
		var biz *BizError
		if errors.As(item.Err, &biz) {
			return biz, true
		}
	}
	return nil, false
}
