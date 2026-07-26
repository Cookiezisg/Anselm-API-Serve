---
id: DOC-035
type: working
status: active
owner: @weilin
created: 2026-07-27
reviewed: 2026-07-27
review-due: 2026-10-25
audience: [human, ai]
landed-into:
---

# 生成能力(图 / TTS):网关侧工单 —— 对应主仓 WRK-082 批B/批C

> **主战役文档在 Anselm 主仓** `docs/working/multimodal-output/README.md`(WRK-082,P1–P19 已拍板)。
> 本页只承载**网关侧**的契约草案与不变量草案;施工时按本仓纪律走(GW-INV 当验收、doc-code parity、
> `make verify` 全绿),landed 时并入 references 四契约 + invariants.md + ADR。
>
> 用户已授权(主仓 P19):真钱实测随便烧;两仓未上线零历史包袱;本地全绿且有把握时 push main
> (=自动部署)可行。

## 1. 要建什么(两个端点,一个原则)

| 端点 | 上游 | 免费档配额(P8) | 计费单位 |
|---|---|---|---|
| `POST /v1/images/generations` | DashScope **同步形**(主仓代拍 B1,官方推荐):`POST https://{DASHSCOPE_WORKSPACE_ID}.<region>.maas.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation`,qwen-image-2.0 系,直接返 **24h OSS URL**——免任务轮询层;上游连接持有几十秒,该路由单独设宽上游超时 | 10 张/天/install | 按张(照 `InputAudioSeconds` 非 token 单位先例) |
| `POST /v1/audio/speech` | DashScope qwen3-tts-instruct-flash(HTTP 非实时) | 5 万字符/天/install | 按字符 |

视频**不进免费档**(P8):网关不开视频路由,零改动。

**一个原则(P13,URL 直通)**:请求取 OpenAI-compat 形;**响应主形是上游 OSS 签名 URL 直通**
(b64 兜底)。理由:本机公网出方向 1Mbps,1–3MiB base64 内联 = 11–30s/张;DashScope 生成结果
本就是 24h 有效、不含 key 的 OSS 签名 URL,直通让客户端直连上游下载,水管零占用。

## 2. GW-INV 新不变量草案(施工时正式编号入 invariants.md,守卫测试钉死)

1. **直通 URL 无密**:返回给客户端的任何上游 URL 可证明不含 API key / 凭证成分(测试断言,不靠口头)。
2. **计费点在生成成功,不在客户端下载**:任务成功取到结果即 settle(URL 给出去没人下载也照扣);
   提交后的 timeout/error 等歧义结果照既有 full quote settle 原则。
3. 既有红线全部照守:key 不出端、双闸原子预留、journal 只增、admin 面不上公网、
   错误原文不透传(归一既有粗粒度枚举)。

## 3. 契约草案(施工时以真 key 实测为准定稿,再入 references/backend/api.md)

- 请求(images):`{model?, prompt, size?, n=1}`——`n>1` 拒(既有关闭联合类纪律);size 由主仓
  三值枚举翻译后的具体值或缺省 1024×1024。
- 响应(images):`{created, data:[{url}]}`;错误走既有 error envelope 与 code 闭集,新增码逐个登记。
- 请求(speech):`{model?, input, voice="Cherry", format?}`;响应:音频 URL(直通)或字节(兜底)。
- 能力面:`Image.RouteProfile` / `Speech.RouteProfile`——Available = key && enabled 双半才真
  (`Multimodal.Available` 先例第二、三次应用);`IMAGE_ENABLED` / `SPEECH_ENABLED` 配置项
  (非 secret,入 config.Specs)。
- 认证/配额/journal:device-proof 逐请求签名、per-install 日限 + 月度钱包双闸、既有轨道零新机制。

## 4. 施工序(跟主仓批次走)

1. **批B 第 0 步**:✅ 文档半已完成(2026-07-27,四家官方文档核准——DashScope 同步形/入参
   messages 形/`parameters.size,n`/2.0 系 512²–2048² 总像素/URL 24h);⏸ 真 key 线缆半待主仓
   代拍 B2 解锁(SSH 被权限闸拒、本地无 key),解锁后真出一张验 URL 无 key/时延/journal。
2. 批B:images 端点(domain → app → infra → transport → 测试 → ref 文档,GW-INV 验收)。
3. 批C:speech 端点(同模子)。
4. landed:契约入 references 四索引、不变量正式编号、URL 直通落本仓 ADR、本页填 landed-into。
