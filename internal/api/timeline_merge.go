package api

import (
	"context"
	"strconv"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/store"
	"moorage/internal/updater"
)

// One action through Moorage often leaves several rows: an audit row (R1), a stack event
// and a version or an image update record. The timeline shows such an action once. These
// pairs are merged, always only when both kinds are requested:
//
//   - stack event -> version (same user, kinds create→create, edit→edit|restore,
//     delete→delete, rollback→restore): the version entry keeps its diff and Restore and
//     gets the event kind;
//   - stack event "update" -> image update record (same actor): the update entry keeps
//     its result and rollback link;
//   - audit row -> stack event (same user, actions in auditEventKinds): the event (or the
//     version/update it was merged into) gets the audit actor, IP and failed outcome;
//   - audit row image.auto_update "update #N" -> image update N, by id.
//
// Rows of one pair must be at most timelineWindow apart, and they must be each other's
// nearest candidate (mutual nearest). That rule depends only on the rows themselves, not on
// the page, so a pair split over two pages is merged the same way on both.
//
// Not merged, but left out on purpose: stack events of kind "terminal" and audit rows
// terminal.open, because the terminal session entry says the same.

var eventVersionKinds = map[string][]string{
	"create":   {engine.KindCreate},
	"edit":     {engine.KindEdit, engine.KindRestore},
	"delete":   {engine.KindDelete},
	"rollback": {engine.KindRestore},
}

var auditEventKinds = map[string][]string{
	"stack.create":     {"create"},
	"stack.save":       {"edit"},
	"stack.delete":     {"delete"},
	"stack.up":         {"deploy"},
	"stack.update":     {"update"},
	"stack.restart":    {"restart"},
	"stack.stop":       {"stop"},
	"stack.down":       {"down"},
	"stack.pull":       {"pull"},
	"stack.restore":    {"rollback", "edit"},
	"image.change_tag": {"edit"},
	"image.update":     {"update"},
	"image.rollback":   {"rollback"},
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pairing context: the rows of each source within this many ms around the page.
var pairSpan = 2 * timelineWindow.Milliseconds()

// mergeDuplicates marks absorbed items (it.into) among cands and folds their facts into
// the entry they were merged into. It loads the rows just outside the page as context, so
// a pair that straddles the page edge is recognised.
func (s *Server) mergeDuplicates(ctx context.Context, q tlQuery, cands []*tlItem, versions []engine.Version) error {
	if len(cands) == 0 || !q.kinds[tlEvent] && !q.kinds[tlAudit] {
		return nil
	}
	lo := cands[len(cands)-1].key.at - pairSpan
	hi := cands[0].key.at + pairSpan
	if q.cursor.at != 0 {
		hi = q.cursor.at + pairSpan
	}
	byKind := map[string][]*tlItem{}
	seen := map[string]bool{}
	put := func(it *tlItem) {
		if k := it.key.String(); !seen[k] {
			seen[k] = true
			byKind[it.key.kind] = append(byKind[it.key.kind], it)
		}
	}
	for _, it := range cands {
		put(it)
	}
	r := store.TimelineRange{HostID: q.hostID, Stack: q.stack, FromMs: lo, ToMs: hi, Limit: q.limit + 256}
	pairVE := q.kinds[tlVersion] && q.kinds[tlEvent]
	pairUE := q.kinds[tlUpdate] && q.kinds[tlEvent]
	pairAE := q.kinds[tlAudit] && q.kinds[tlEvent]
	if q.kinds[tlEvent] && (pairVE || pairUE || pairAE) {
		rows, err := s.store.TimelineEvents(ctx, r, "terminal")
		if err != nil {
			return err
		}
		for _, it := range eventItems(rows) {
			put(it)
		}
	}
	if pairVE {
		for _, it := range versionItems(versions) {
			if it.key.at >= lo && it.key.at <= hi {
				put(it)
			}
		}
	}
	if pairUE {
		rows, err := s.store.TimelineImageUpdates(ctx, r)
		if err != nil {
			return err
		}
		for _, it := range updateItems(rows) {
			put(it)
		}
	}
	if pairAE {
		rows, err := s.store.TimelineAudit(ctx, r, auditTermOpen)
		if err != nil {
			return err
		}
		for _, it := range auditItems(rows) {
			put(it)
		}
	}

	events := byKind[tlEvent]
	if pairVE {
		pairMutual(events, byKind[tlVersion], func(e, v *tlItem) bool {
			return e.actor == v.actor && contains(eventVersionKinds[e.class], v.class)
		})
	}
	if pairUE {
		pairMutual(events, byKind[tlUpdate], func(e, u *tlItem) bool {
			return e.class == "update" && e.actor == u.actor
		})
	}
	if pairAE {
		pairMutual(byKind[tlAudit], events, func(a, e *tlItem) bool {
			return a.actor == e.actor && contains(auditEventKinds[a.class], e.class)
		})
	}
	if q.kinds[tlAudit] && q.kinds[tlUpdate] {
		if err := s.absorbAutoUpdates(ctx, q, byKind[tlAudit]); err != nil {
			return err
		}
	}
	// A fixed kind order keeps Merged in the same order on every page.
	for _, kind := range timelineKinds {
		for _, it := range byKind[kind] {
			if it.into != nil {
				fold(it.root(), it)
			}
		}
	}
	return nil
}

// pairMutual sets a.into = b for every a and b that are each other's nearest match within
// timelineWindow. Ties go to the newer row, so the result never depends on input order.
func pairMutual(as, bs []*tlItem, match func(a, b *tlItem) bool) {
	win := timelineWindow.Milliseconds()
	nearest := func(x *tlItem, ys []*tlItem, ok func(y *tlItem) bool) *tlItem {
		var best *tlItem
		var bestD int64
		for _, y := range ys {
			d := x.key.at - y.key.at
			if d < 0 {
				d = -d
			}
			if d > win || !ok(y) {
				continue
			}
			if best == nil || d < bestD || d == bestD && y.key.cmp(best.key) > 0 {
				best, bestD = y, d
			}
		}
		return best
	}
	for _, a := range as {
		if a.into != nil {
			continue
		}
		b := nearest(a, bs, func(b *tlItem) bool { return match(a, b) })
		if b == nil {
			continue
		}
		if nearest(b, as, func(x *tlItem) bool { return match(x, b) }) == a {
			a.into = b
		}
	}
}

// absorbAutoUpdates hides the audit row the scheduler writes after an auto-update
// ("update #N"): it is written when the update ends, often minutes after its start, so it
// is linked by id instead of by time. A row whose update record is gone (pruned) stays.
func (s *Server) absorbAutoUpdates(ctx context.Context, q tlQuery, audits []*tlItem) error {
	for _, a := range audits {
		if a.class != updater.AuditAutoUpdate || a.into != nil {
			continue
		}
		n, ok := strings.CutPrefix(a.entry.Detail, "update #")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			continue
		}
		u, err := s.store.ImageUpdateByID(ctx, id)
		if err != nil || u.HostID != q.hostID || u.Stack != q.stack {
			continue
		}
		// A placeholder target: the update entry needs nothing from this row, but the row
		// must not be listed on its own.
		a.into = &tlItem{key: tlKey{kind: tlUpdate}}
	}
	return nil
}

// fold copies the facts of an absorbed row into the entry that is shown.
func fold(dst, src *tlItem) {
	d := &dst.entry
	switch src.key.kind {
	case tlEvent:
		d.Merged = appendOnce(d.Merged, tlEvent)
		d.Ref.EventID = src.entry.Ref.EventID
		if d.EventKind == "" {
			d.EventKind = src.entry.EventKind
		}
		if d.Kind == tlVersion && src.entry.EventKind == "rollback" {
			d.Title = "Rolled back and deployed"
		}
	case tlAudit:
		d.Merged = appendOnce(d.Merged, tlAudit)
		d.Ref.AuditID = src.entry.Ref.AuditID
		d.Action = src.entry.Action
		d.Actor = src.entry.Actor
		d.IP = src.entry.IP
		if src.entry.Outcome == store.AuditFailed && d.Outcome == "" {
			d.Outcome = store.AuditFailed
		}
	}
}

func appendOnce(list []string, s string) []string {
	if contains(list, s) {
		return list
	}
	return append(list, s)
}
