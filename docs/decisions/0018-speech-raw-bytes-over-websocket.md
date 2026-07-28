---
id: DOC-035
type: decision
status: active
owner: @weilin
created: 2026-07-28
reviewed: 2026-07-28
review-due: 2099-12-31
audience: [human, ai]
---

# 0018 — 受管语音改走双工 WebSocket,响应体是裸音频

> 收窄桌面仓 ADR 0012 保留的 P13「URL 直通」在**语音**上的适用范围。
> 图像与视频**不动**——那两家上游真的返 URL。

## 背景 / Context

`POST /v1/audio/speech` 此前返 `{created, data:[{url}]}`,与出图同形,理由写在 handler 头上:
P13 让 URL 直通成为网关的整个媒体契约,网关**从不持有产物字节**。

WRK-082 H9 要把参考音色做进免费档,于是要选一个能用**克隆音色**合成的模型。逐家实测后只剩一个:

| 模型 | 预置音色 | 克隆音色 | 传输 |
|---|---|---|---|
| `qwen3-tts-flash`(原用) | ✅ | ❌ | HTTP |
| `cosyvoice-v2` | — | — | 新加坡区**不存在** |
| **`qwen-audio-3.0-tts-flash`** | ✅ | ✅ | **只有 WebSocket** |

两条 HTTP 形状对新模型都答 `url error, please check url`(真机实测 2026-07-28,工作区专属域名上)。

## 判别 / What forced this

**一条双工流回来的是帧,不是产物 URL。** P13 的 URL 直通在这里**没有东西可以直通**——不是我们
不愿意遵守,是那个约定的前提(上游给一个可转发的地址)在这条路径上不存在。

于是只有两个选择:

| 方案 | 代价 |
|---|---|
| A. 响应体就是音频字节 | 契约变了 |
| B. 字节停进媒体库、签一个 lease 返 URL | 存储 + 过期 + 清理 + 第二次往返,**而网关照样持有字节** |

B 保住的只是一个 JSON 信封的形状,买单的是四样真机制;而它连 P13 的**实质**(网关不持有字节)
都保不住——那个实质在双工上游面前已经不可得了。

## 决定 / Decision

**`POST /v1/audio/speech` 返回裸 `audio/wav` 字节。**

1. **这是 OpenAI 在这条端点上自己的形状**,故它是**更**标准、不是更不标准。此前「与兄弟生成端点
   同形,客户端只认一种形状」的理由,是在两者都返 URL 时成立的;现在语音与图像**真的不同**,
   强行同形等于造机器去掩盖一个真实差异。
2. **P13 对图像与视频原样有效**——那两家上游返 URL,直通仍是对的。
3. **模型收敛顺带成立**:同一个模型既做预置音色又做克隆音色,故「音色合成与普通语音走同一配额」
   不是硬凑的产品决定,而是**同一个模型的同一次调用**这一物理事实。
4. **端点从凭证 origin 派生**(`inferenceWSURL`),不写常量——区域是 key 的属性,写死主机曾真的
   把一把新加坡 key 送去北京(H0)。
5. **计费判别按「有没有字节流出来」**:`task-failed` 在任何音频之前 = 可证明未计费,可回滚;
   一旦有字节,上游已经干了活,计费保留(GW-INV-50)。空的 `task-finished` 视为**歧义**、保留计费
   ——上游可能已合成已计费而我们没收到。

## 代价与取舍

- 桌面侧受管路径少了一次往返(此前是「拿 URL → 下载」),也少了一个「短时签名 URL 在铸出与取用
  之间过期」的地方。
- 网关在一次合成期间持有整段音频。上限由 `maxAudioBytes`(32MiB)守——线缆本就把输入封在
  `maxInputChars`,故这是**失控闸**、不是产品上限:一个永远不发 `task-finished` 的上游不得无界
  撑大网关内存。
- 放弃的是「语音产物由 provider 直供、网关零出口流量」。它只在 HTTP 那条路上存在过,而那条路
  用不了克隆音色。

## 落点

- `internal/infra/upstream/ttsgen.go`(整文件重写为双工 client)+ `ttsgen_test.go`(协议顺序与
  三种结局的计费判别逐条钉死)
- `internal/app/tts/service.go` · `handlers/business/audio/handler.go`
- 桌面 `backend/internal/infra/llm/speechgen.go` 受管分支
- `references/backend/api.md` · `config.md`
