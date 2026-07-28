// Package voicestore persists the per-install cloned-voice inventory (WRK-082 H9). It structurally
// satisfies the app/voice Store port and never imports app.
//
// **Create is a transaction for one reason: the ceiling check and the INSERT must not straddle a
// window.** Both ceilings are read by the service before it spends money upstream, but between that
// read and this write another request can land — so the transaction re-reads inside BEGIN IMMEDIATE
// and the UNIQUE index catches the name race the count cannot. Losing here costs a rollback of an
// already-paid registration; losing WITHOUT this costs a permanently unaddressable orphan.
//
// Package voicestore 持久化逐 install 的克隆音色库存(H9)。它结构化满足 app/voice 的 Store 端口,
// 且从不 import app。
//
// **Create 是一个事务,理由只有一个:上限检查与 INSERT 之间不能跨着一个窗口。** 两条上限都由 service
// 在花钱之前读过,但那次读与这次写之间会落进另一个请求——故事务在 BEGIN IMMEDIATE 里**重读一次**,而
// UNIQUE 索引接住计数接不住的那个重名竞态。在这里输掉的代价是回滚一次已付费的登记;**没有**这一层而
// 输掉的代价,是一个永远寻址不到的孤儿。
package voicestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/pkg/orm"

	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
)

// Store holds the writer (serialized BEGIN IMMEDIATE) and reader (concurrent WAL) pools.
//
// Store 持有写池(串行 BEGIN IMMEDIATE)与读池(并发 WAL)。
type Store struct {
	w *orm.DB
	r *orm.DB
}

// New builds a Store over the given pools.
func New(writer, reader *orm.DB) *Store {
	return &Store{w: writer, r: reader}
}

// ListVoices returns one install's voices, oldest first — stable order so the two rows a user is
// allowed to keep do not swap places between two reads of the same unchanged inventory.
//
// ListVoices 返回一个 install 的音色,旧的在前——**稳定顺序**,使用户能留的那两行不会在同一份没变过的
// 库存的两次读之间互换位置。
func (s *Store) ListVoices(ctx context.Context, installID string) ([]domvoice.Voice, error) {
	rows, err := s.r.Query(ctx, `SELECT id,name,upstream_id,created_at FROM install_voices
		WHERE install_id=? ORDER BY created_at ASC, id ASC`, installID)
	if err != nil {
		return nil, fmt.Errorf("voicestore.ListVoices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// A non-nil empty slice: an install with no voices must serialise as [] rather than null.
	// 非 nil 的空切片:没有音色的 install 必须序列化成 [] 而非 null。
	out := make([]domvoice.Voice, 0, domvoice.PerInstallInventory)
	for rows.Next() {
		var v domvoice.Voice
		var created int64
		if err := rows.Scan(&v.ID, &v.Name, &v.UpstreamID, &created); err != nil {
			return nil, fmt.Errorf("voicestore.ListVoices scan: %w", err)
		}
		v.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("voicestore.ListVoices rows: %w", err)
	}
	return out, nil
}

// CountAllVoices is the account-wide total — the number only this gateway can know, because every
// install's clone lives under one provider credential.
//
// CountAllVoices 是账号级总数——**只有本网关知道**的那个数,因为每个 install 的克隆都住在同一把
// provider 凭证之下。
func (s *Store) CountAllVoices(ctx context.Context) (int, error) {
	var n int
	if err := s.r.QueryRow(ctx, `SELECT COUNT(*) FROM install_voices`).Scan(&n); err != nil {
		return 0, fmt.Errorf("voicestore.CountAllVoices: %w", err)
	}
	return n, nil
}

// CreateVoice records a registration, re-checking the per-install ceiling inside the transaction.
//
// CreateVoice 记下一次登记,并在事务内**重查**逐 install 上限。
func (s *Store) CreateVoice(ctx context.Context, installID string, v domvoice.Voice) error {
	return s.w.Transaction(ctx, func(tx *orm.DB) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM install_voices WHERE install_id=?`, installID).Scan(&n); err != nil {
			return fmt.Errorf("voicestore.CreateVoice count: %w", err)
		}
		if n >= domvoice.PerInstallInventory {
			return domvoice.ErrInventoryFull
		}
		_, err := tx.Exec(ctx, `INSERT INTO install_voices (id,install_id,name,upstream_id,created_at)
			VALUES (?,?,?,?,?)`, v.ID, installID, v.Name, v.UpstreamID, v.CreatedAt.UTC().Unix())
		if err != nil {
			// The UNIQUE(install_id,name) index is the name race's only catcher — two concurrent
			// enrollments of one name both pass the service's pre-check.
			// UNIQUE(install_id,name) 索引是重名竞态**唯一**的接手者——同一个名字的两次并发登记,在
			// service 的前置检查里都会通过。
			if isUnique(err) {
				return domvoice.ErrNameTaken
			}
			return fmt.Errorf("voicestore.CreateVoice: %w", err)
		}
		return nil
	})
}

// DeleteVoice removes one row, returning its upstream id. Ownership-enforcing: another install's id
// reads as absent, so voice ids never become an existence oracle.
//
// DeleteVoice 删掉一行并返回它的上游 id。**强制归属**:别的 install 的 id 读作不存在,故音色 id 永远
// 不会变成一个存在性预言机。
func (s *Store) DeleteVoice(ctx context.Context, installID, id string) (string, bool, error) {
	var upstreamID string
	err := s.r.QueryRow(ctx, `SELECT upstream_id FROM install_voices WHERE id=? AND install_id=?`,
		id, installID).Scan(&upstreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("voicestore.DeleteVoice lookup: %w", err)
	}
	if _, err := s.w.Exec(ctx, `DELETE FROM install_voices WHERE id=? AND install_id=?`, id, installID); err != nil {
		return "", false, fmt.Errorf("voicestore.DeleteVoice: %w", err)
	}
	return upstreamID, true, nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
