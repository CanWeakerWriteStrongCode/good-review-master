# 提示词离线评测集（tests/eval）

**这个目录回答的问题**：改了 prompt、换了模型、动了选窗逻辑之后，回复质量有没有变差？

单元测试管不了这件事——模型输出没有唯一正确答案，`assert.Equal` 断言不了。所以这里的做法是：

1. 按**与线上逐字相同**的提示词调一次真实大模型；
2. 跑几条**规则检查**（长度上下限、必须/禁止出现的串）——这些是硬失败；
3. 与 `baseline.json` 逐行比对，把差异打出来**给人看**——模型输出天然会变，差异只报告、不判失败。

它是测试设施，不参与运行时装配，所以住在 `tests/` 下（与 `tests/e2e` 并列）。

## 跑法

在**仓库根目录**执行（用例里的路径都相对根目录）：

```bash
# 跑一遍：规则检查 + 与基线比对
go run ./tests/eval

# 第一次用、或者确认这次的变化就是要的：把本次输出写成新基线
go run ./tests/eval -update

# 只解析并打印将要发送的提示词，不调大模型（调试用例本身时用）
go run ./tests/eval -dry
```

常用参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-config` / `-secret` | `config.yaml` / `secret.yaml` | 复用主程序的配置（模型名、采样参数、API Key 都从这里读） |
| `-prompt` | `prompt_system.yaml` | 提示词来源；用例通过 `category` + `keyword` 从里面取 |
| `-cases` | `tests/eval/cases` | 用例目录，读其中的 `*.yaml` |
| `-baseline` | `tests/eval/baseline.json` | 基线文件 |
| `-nickname` | `机器人` | 评测用的机器人昵称（真实昵称来自 NapCat，离线拿不到） |
| `-timeout` | `120s` | 单条用例超时 |

退出码：**有规则检查失败 = 1**，全部通过 = 0。差异（与基线不同）不改变退出码。

## 为什么提示词是"引用"而不是"内联"

`prompt_system.yaml` / `prompt_custom.yaml` 是**产品本体**，也就是最该防回归的东西。
用例里写 `category` + `keyword`，runner 通过 `router.ComposeSystemPrompt` 与
`router.BuildUserMsg` 组装出提示词——线上用的是同一对函数。

如果评测自己抄一份提示词进 YAML，那就会出现最没用的一种结果：**评测全绿，线上照样回归**。

代价是用例与你的提示词配置耦合：`catgirl-basic` 引用的 `猫娘` 来自
`config/prompt_system_example.yaml`，你把关键字改了就要同步改用例。
runner 在找不到关键字时会把当前可选的类别/关键字**全部列出来**，照着改即可。

## 加一条用例

在 `tests/eval/cases/*.yaml` 里追加一项：

```yaml
cases:
  - id: 唯一标识                       # 用作基线的 key，全局不能重复
    note: 这条用例想验证什么             # 只给人看
    prompt:
      category: chat_review           # prompt_system.yaml 里的类别
      keyword: "猫娘"                  # 该类别下的关键字
    input: |                          # 群聊记录，格式与发给大模型的一致（每行一条 JSON）
      {"msg_id":1001,"user":"张三","user_id":123456,"content":"今天好累"}
    checks:
      - kind: max_runes
        value: 400
        note: 规则要求 300 字以内，留余量
```

支持的检查：

| kind | value | 含义 |
| --- | --- | --- |
| `max_runes` | 整数 | 输出字数上限 |
| `min_runes` | 整数 | 输出字数下限（通常是"别返回空串"） |
| `contains` | 字符串 | 输出里必须出现 |
| `not_contains` | 字符串 | 输出里不许出现 |

## 基线（baseline.json）

第一次跑完用 `-update` 生成，**提交入库**——正是它让"这次输出和上次不一样"变得可见。

每条记录里有四个字段是用来解释差异来源的：

| 字段 | 作用 |
| --- | --- |
| `prompt_sha256` | **提示词版本化**：实际发出去的 system prompt + user message 的指纹 |
| `model` | 换了模型之后整份基线都该重新生成，这个字段提醒你这一点 |
| `prompt_tokens` / `completion_tokens` | 服务端返回的真实用量 |
| `recorded_at` | 这条基线是什么时候记的 |

`prompt_sha256` 是这套机制里最要紧的一个：没有它，输出变了只能看到"不一样了"，
分不清是**我改了提示词**还是**上游模型漂了**——而这两种情况的处置完全相反（一个要
review 这次改动，一个要查上游）。报告里会在指纹与基线不同时直接写明"差异由提示词变化解释"。

另外两条：

- 调用失败（网络/鉴权）时**不会**写基线：把一次故障固化成"模型的正常表现"，
  之后所有比对都会失去意义。
- 上游不返回 `usage` 时用量显示"上游未返回 usage"，而不是 0——两者含义不同。
- 老基线里没有 `prompt_sha256` 时不做提示词变化判断（避免把升级本身报成"提示词变了"），
  跑一次 `-update` 补上即可。

## 已知边界

- 只覆盖"命中关键字"的指令路由。未命中关键字时的纯 @ 聊天走 `replyDefault`，
  它没有 `category`/`keyword` 可引用，目前不在评测范围内。
- 规则检查只能挡住"空回复 / 超长 / 出现助手腔"这类明显退化。
  要判"回复得聪不聪明"仍得人看 diff——本目录刻意不上 LLM-as-judge，
  那需要先有一批标注样本，否则只是把不确定性从一个模型搬到另一个模型。
