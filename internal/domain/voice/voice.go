// Package voice holds the cloned-voice vocabulary shared by the use case and its store (WRK-082 H9).
//
// The two sentinels here exist because the SAME two refusals arise in two places that cannot see
// each other: the service checks both preconditions before it spends money, and the store's
// transaction re-checks them inside BEGIN IMMEDIATE where a concurrent enrollment is finally
// visible. A caller must not be able to tell which layer refused — losing a race and arriving late
// are the same fact about the world, and one wire code says it.
//
// Package voice 持有用例与其 store 共用的克隆音色词汇(H9)。
//
// 这里的两个 sentinel 存在,是因为**同样的两条拒绝**出现在两个互相看不见的地方:service 在花钱之前
// 查两个前置条件,而 store 的事务在 BEGIN IMMEDIATE 里**重查**一遍——并发登记只有在那里才终于可见。
// 调用方不该分辨得出是哪一层拒的:输掉竞态与来晚了,是关于世界的**同一个事实**,由同一个 wire code 说出。
package voice

import (
	"errors"
	"time"
)

// PerInstallInventory is how many voices one install may keep (用户拍板 2026-07-28: 2).
//
// It is INVENTORY, not quota: nothing frees a slot with the passage of time, so every refusal that
// cites it must name deletion as the remedy.
//
// PerInstallInventory 是一个 install 能留几个音色(用户 2026-07-28 拍板:2)。
//
// 它是**库存、不是配额**:时间流逝不腾位,故一切引用它的拒绝都必须点明「删一个」才是补救办法。
const PerInstallInventory = 2

// Voice is one enrolled voice as this gateway records it. The upstream id is the only handle to a
// registration living in OUR provider account — lose it and the voice becomes unreclaimable.
//
// Voice 是本网关记下的一个已登记音色。上游 id 是那个住在**我们** provider 账号里的登记的**唯一**把手
// ——丢了它,这个音色就再也收不回来。
type Voice struct {
	ID         string
	Name       string
	UpstreamID string
	CreatedAt  time.Time
}

// ErrInventoryFull / ErrNameTaken: the two refusals, raised by the service's pre-check and again by
// the store's transaction.
//
// ErrInventoryFull / ErrNameTaken:那两条拒绝,由 service 的前置检查提出、又由 store 的事务再提一次。
var (
	ErrInventoryFull = errors.New("voice: inventory full")
	ErrNameTaken     = errors.New("voice: name taken")
)
