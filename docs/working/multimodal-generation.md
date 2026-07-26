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

## 3.5 批B 施工契约定稿(2026-07-27 读码扇出后;实现照此机械施工)

**config(`domain/config/spec.go` 既有 Spec 机制)**:
- `IMAGE_ENABLED`(与 `MEDIA_ENABLED` 同 tier;bool,默认 false)
- `IMAGE_UPSTREAM_MODEL`(TierStartupHard,默认 `qwen-image-2.0`)
- `IMAGE_DAILY_LIMIT`(TierRuntimeHot,默认 10,0=关)
- `DASHSCOPE_NATIVE_BASE`(TierStartupHard,默认 `https://dashscope.aliyuncs.com`——multimodal-generation
  是原生 DashScope API,**不走**既有 compatible-mode 的 `QwenBaseURL`;workspace host 形实测后再定)
- `IMAGE_UPSTREAM_TIMEOUT_SEC`(默认 120——同步上游连接持有几十秒)

**billing**:`InputImages` 追加进 InputClass 枚举(wire 冻结形增量);模型常量 `QwenImage20`;
按张 rate card(**价格待实测对账**,先按 P8 工作假设 ¥0.25/张 ≈ $0.035 = 35_000_000_000 pUSD/张,
注释钉死「上线前对官方价页对账」,主仓代拍 B3);`NewImagesPlan(provider, model, n)` +
`(Plan).ImagesCost(n)`(镜像 AudioSeconds 对;n=1 时 reserve==settle,确定性成本)。

**quota(代拍 B4:品类日闸一个机制吃图与 TTS 两批)**:
- 迁移 `0006_install_category_daily.sql`:
  `install_category_daily(install_id, category, period_day, units, PRIMARY KEY(install_id,category,period_day))`
- `Limits` + `ImageDailyLimit`;`Reservation` + `CategoryApplied string` + `CategoryUnits int64`
  (rollback 凭预留时记录反转,不重读 live config——同 `SublimitApplied` 纪律)
- store.Reserve:`plan.InputClass==InputImages` 时在**同一** BEGIN IMMEDIATE 里
  `UPDATE ... SET units=units+? WHERE ... AND units+? <= lim.ImageDailyLimit`,
  拒 → `quota.ErrCategoryDailyExceeded` → 新 apierr `IMAGE_QUOTA_EXHAUSTED`(429)

**app/image(克隆 speech 形)**:ports Authenticator/RateLimiter/Config/Quota/Clock/Metrics +
Upstream 端口 `Generate(ctx, prompt, size, model) (url string, err error)`;
Service:Authorize → NewImagesPlan(1) → Reserve → [transport 调 upstream] → 成功 Settle 全额 /
可证明未计费的拒绝 Rollback;可用性 = `len(QwenAPIKeys)>0 && ImageEnabled`(双半)。

**infra(upstream 侧)**:DashScope 同步 client——
`POST {DASHSCOPE_NATIVE_BASE}/api/v1/services/aigc/multimodal-generation/generation`,
body `{model, input:{messages:[{role:"user",content:[{text:prompt}]}]}, parameters:{size,n:1,watermark:false}}`,
解析 `output.choices[0].message.content[].image` 取 URL;错误归一既有粗粒度枚举、原文不透传;
key 仅注入 cloned request(既有铁律)。

**transport `business/images/handler.go`**:`POST /v1/images/generations`(proof.Protect);
请求 `{model?, prompt, size?, n?}`——`n>1` 422 拒(关闭联合类)、prompt 非空且 ≤2000 字符、
`size` 为 `WxH` 形且总像素 ∈ [512², 2048²](默认 `1024*1024`);
响应 `{created, data:[{url}]}`;journal/metrics 低基数 label 照 chat 惯例。

**能力面**:`AnselmCapabilities`(version 1 增量字段)+
`ImageGeneration *GenProfile{Available bool, DailyLimit int64}`——不硬套 token 语义的 RouteProfile;
老客户端解码器忽略未知字段、老网关缺字段=nil=不可用,滚动兼容自然成立。

**错误码闭集新增**:`IMAGE_UNAVAILABLE`(503)、`IMAGE_QUOTA_EXHAUSTED`(429)——error-codes.md 登记。

**GW-INV 新不变量(施工时按登记册取正式编号)**:
1. 直通 URL 不含任何已配置 upstream API key(机械测试:URL 串与全部已配 key 逐一取交)——
   注意 OSS 签名 URL 携带 `OSSAccessKeyId`(OSS 临时访问标识,非 DashScope key),不变量表述必须精确到「网关配置的 key」。
2. 生成成功即 settle 全额;客户端是否下载与计费无关;歧义结果 full quote settle。
3. category daily units 预留-回滚对称(凭 Reservation 快照,不重读 live config)。

**测试**:billing(Plan Validate 往返/ImagesCost)、quotastore(category gate 原子 + 回滚反转 +
月/日/钱包既有门不回归)、app/image service(授权/双半可用性/结算回滚)、handler(校验拒绝/信封)、
e2e 假上游全链;真上游冒烟待 B2 解锁。

## 4. 施工序(跟主仓批次走)

1. **批B 第 0 步**:✅ 文档半已完成(2026-07-27,四家官方文档核准——DashScope 同步形/入参
   messages 形/`parameters.size,n`/2.0 系 512²–2048² 总像素/URL 24h);⏸ 真 key 线缆半待主仓
   代拍 B2 解锁(SSH 被权限闸拒、本地无 key),解锁后真出一张验 URL 无 key/时延/journal。
2. 批B:images 端点(domain → app → infra → transport → 测试 → ref 文档,GW-INV 验收)。
3. 批C:speech 端点(同模子)。
4. landed:契约入 references 四索引、不变量正式编号、URL 直通落本仓 ADR、本页填 landed-into。
