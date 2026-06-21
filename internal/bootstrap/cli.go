package bootstrap

// cli.go is the localhost-only ban/unban admin CLI (ported from
// _legacy/cmd/server/admin.go). It runs AS A CLI on the box (the binary binds
// loopback at the network layer; ops reach it over SSH, never the public
// surface), opens the SAME write pool the server uses, and flips installs.status
// with a structured audit WARN — install_id + operator reason + outcome, NEVER a
// token/secret (install_id is an opaque ins_<hex>, not a credential). cmd/gateway
// imports only bootstrap, so the dispatch lives here.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/infra/configprovider"
	"github.com/sunweilin/anselm/gateway/internal/infra/sqlite"
	"github.com/sunweilin/anselm/gateway/internal/pkg/logx"
)

// maxReasonChars bounds the operator --reason logged verbatim so one audit line
// cannot bloat journald; longer is truncated, never rejected (the audit must land).
const maxReasonChars = 256

// RunCLI dispatches the ban/unban subcommands. It returns handled=true (and the
// process exit code) when it consumed the args, so main exits; a non-admin first
// arg returns handled=false so main proceeds to serve.
func RunCLI(args []string, getenv func(string) string) (handled bool, code int) {
	if len(args) < 2 {
		return false, 0
	}
	switch args[1] {
	case "ban":
		logx.InitDefault() // same JSON + redaction floor the server emits.
		return true, runBan(args[2:], getenv)
	case "unban":
		logx.InitDefault()
		return true, runUnban(args[2:], getenv)
	default:
		return false, 0
	}
}

// runBan parses `gateway ban <install_id> --reason <r>` (id either side of the
// flag) and flips status to banned. --reason is mandatory + audited.
func runBan(args []string, getenv func(string) string) int {
	id, reason, ok := parseIDAndReason("ban", args)
	if !ok {
		return 2
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: gateway ban <install_id> --reason <reason>")
		return 2
	}
	if reason == "" {
		fmt.Fprintln(os.Stderr, "ban requires --reason (audited)")
		return 2
	}
	return adminMutate("ban", id, reason, "banned", getenv)
}

// runUnban parses `gateway unban <install_id>` (optional --reason) and flips back
// to active.
func runUnban(args []string, getenv func(string) string) int {
	id, reason, ok := parseIDAndReason("unban", args)
	if !ok {
		return 2
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: gateway unban <install_id> [--reason <reason>]")
		return 2
	}
	return adminMutate("unban", id, reason, "active", getenv)
}

// parseIDAndReason extracts install_id + --reason, accepting the id BEFORE or
// AFTER the flag (flag.Parse stops at the first positional), so both documented
// orderings work without mis-reading a flag VALUE as the id.
func parseIDAndReason(name string, args []string) (id, reason string, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	r := fs.String("reason", "", "human reason (audited)")

	var rest []string
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		id = args[0]
		rest = args[1:]
	} else {
		rest = args
	}
	if err := fs.Parse(rest); err != nil {
		return "", "", false
	}
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	reason = *r
	if len(reason) > maxReasonChars {
		reason = reason[:maxReasonChars]
	}
	return id, reason, true
}

// adminMutate opens the write pool, flips installs.status, and emits the audit
// line. Shared by ban/unban so both go through one DB + one audit path.
func adminMutate(action, installID, reason, newStatus string, getenv func(string) string) int {
	base, err := configprovider.LoadBase(getenv)
	if err != nil {
		slog.Error("admin_config_load_failed", "action", action, "error", err.Error())
		return 1
	}
	db, err := sqlite.Open(sqlite.Config{
		Path:              base.DBPath,
		CacheKiB:          base.SQLiteCacheKiB,
		MmapBytes:         base.SQLiteMmapBytes,
		WalAutocheckpoint: base.SQLiteAutockptPage,
		ReadPoolMaxConns:  base.ReadPoolMaxConns,
		ConnMaxLifetime:   5 * time.Minute,
	})
	if err != nil {
		slog.Error("admin_store_open_failed", "action", action, "error", err.Error())
		return 1
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := db.Writer.Exec(ctx, `UPDATE installs SET status = ? WHERE id = ?`, newStatus, installID)
	if err != nil {
		slog.Error("admin_action_failed", "event", "admin_audit",
			"action", action, "install_id", installID, "reason", reason, "error", err.Error())
		return 1
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		slog.Warn("admin_action_no_match", "event", "admin_audit",
			"action", action, "install_id", installID, "reason", reason, "outcome", "not_found")
		fmt.Fprintf(os.Stderr, "no install with id %q\n", installID)
		return 1
	}

	// Audit WARN (never sampled): operator + reason + outcome, no token/secret.
	slog.Warn("admin_action", "event", "admin_audit",
		"action", action, "install_id", installID, "reason", reason,
		"new_status", newStatus, "outcome", "ok")
	fmt.Printf("%s ok: %s -> status=%s\n", action, installID, newStatus)
	return 0
}
