// Package poll implements one incremental fetch-and-ingest pass over
// wilmabridge's SQLite store: for each role, work out which messages are
// new — from the DB's per-role high-water mark, or a time window on a
// role's first ever poll — fetch them oldest first, and advance the mark
// per successfully stored message so an interrupted pass always resumes at
// the last contiguous success rather than skipping a gap.
//
// It deliberately depends only on internal/wilma for types, reached
// through the small Source and Sink interfaces below, so the incremental
// logic is testable without HTTP or a real Wilma session. cmd/wilmabridge
// wires the real *wilma.Client and *store.Store in through them.
package poll

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"wilmabridge/internal/wilma"
)

// Role is one child's role, already labelled and filtered by the caller
// (e.g. via --children / --role).
type Role struct {
	Prefix string // Wilma path prefix, e.g. "/!012345"; also the sync_state key
	Name   string // display label
}

// Source is the Wilma side of a pass. It is expressed in wilma.Message
// rather than the client's own list type because that type is unexported;
// cmd/wilmabridge's adapter pairs wilma.Client.ListFolder with
// wilma.ToMessages to satisfy this.
type Source interface {
	// List returns every message currently in the role's inbox, in
	// whatever order Wilma provides them (Pass sorts).
	List(rolePrefix, childName string) ([]wilma.Message, error)
	// FillBody fetches one message's body in place. Fetching a body marks
	// the message read in Wilma — see internal/wilma's package comment.
	FillBody(rolePrefix string, msg *wilma.Message) error
}

// Sink is the DB side of a pass; *store.Store satisfies it.
type Sink interface {
	HighWaterMark(role string) (id int64, ok bool, err error)
	IngestMessage(msg wilma.Message) (isNew bool, err error)
	SetHighWaterMark(role string, wilmaID int64) error
	MarkPolled(role string) error
}

// Options configures a Pass.
type Options struct {
	// Bootstrap is the time window used only on a role's first ever poll
	// (no recorded high-water mark). Zero disables the cutoff — every
	// message in the folder is treated as in-window.
	Bootstrap time.Duration
	// FromNow, on a role's first ever poll, adopts the newest listed ID as
	// the starting mark and fetches nothing — the "don't mark my history
	// read" escape hatch. Bootstrap is ignored when this is set.
	FromNow bool
	// Bodies controls whether message bodies are fetched at all. false is
	// non-invasive (never marks anything read) but leaves every stored
	// message extract_state='skipped' — see cmd/wilmabridge's --bodies=false
	// warning.
	Bodies bool
	// Delay is paused before every HTTP request after the first one in the
	// whole pass.
	Delay time.Duration
	// Now, if set, replaces time.Now (for tests). Nil uses the real clock.
	Now func() time.Time
	// Sleep, if set, replaces time.Sleep (for tests). Nil sleeps for real.
	Sleep func(time.Duration)
	// Log, if set, receives verbose (-v) progress detail. Nil discards it.
	Log func(string, ...any)
	// Warn, if set, receives operational warnings that matter regardless
	// of -v: currently just that a role has no recorded high-water mark
	// and is about to have its bootstrap-window messages' bodies fetched,
	// which marks them read in Wilma. Nil discards it; cmd/wilmabridge
	// always wires this to a real stderr printer.
	Warn func(string, ...any)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) sleep(d time.Duration) {
	if o.Sleep != nil {
		o.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

func (o Options) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

// RoleResult reports what happened for one role during a Pass.
type RoleResult struct {
	Role, Name string
	// Bootstrap is true when this role had no recorded high-water mark at
	// the start of the pass, so the time window (or --from-now) applied.
	Bootstrap bool
	// Listed is how many messages Wilma returned for the role's folder.
	Listed int
	// Selected is how many of those passed the incremental window filter.
	Selected int
	// Ingested is how many were successfully stored this pass. Ingested
	// counts once per role, so a message sent to two children is counted
	// in both roles' Ingested even though it collapses to one messages row.
	Ingested int
	// New is how many of the ones stored were new rows (store.IngestMessage
	// isNew=true) rather than a message this role had already seen.
	New int
	// MarkBefore/MarkAfter are the role's high-water mark before and after
	// this pass (0 if none was ever recorded).
	MarkBefore, MarkAfter int64
	// Err is the first error encountered for this role, if any. It stops
	// further processing of this role only — other roles are unaffected —
	// unless it wraps wilma.ErrSessionExpired, in which case Pass itself
	// returns early with this same error.
	Err error
}

// Result is the outcome of one Pass across every role it attempted.
type Result struct {
	Roles             []RoleResult
	Started, Finished time.Time
}

// Totals sums every role's counters.
func (r Result) Totals() (listed, selected, ingested, newCount, failed int) {
	for _, rr := range r.Roles {
		listed += rr.Listed
		selected += rr.Selected
		ingested += rr.Ingested
		newCount += rr.New
		if rr.Err != nil {
			failed++
		}
	}
	return listed, selected, ingested, newCount, failed
}

// Err joins every role's error into one, or returns nil if none failed.
func (r Result) Err() error {
	var errs []error
	for _, rr := range r.Roles {
		if rr.Err != nil {
			errs = append(errs, fmt.Errorf("role %s: %w", rr.Role, rr.Err))
		}
	}
	return errors.Join(errs...)
}

// Quiet reports whether this pass is worth mentioning: nothing was stored
// and nothing failed. Callers use this to keep a clean poll silent, since
// under cron any output means mail.
func (r Result) Quiet() bool {
	_, _, ingested, _, failed := r.Totals()
	return ingested == 0 && failed == 0
}

// bodyCache holds one message's fetched body for the lifetime of a single
// Pass, keyed by Wilma ID. The same message can be delivered to more than
// one child's role feed with the same ID (see internal/store's dedup); this
// cache halves the HTTP requests for a shared message and guarantees an
// identical body — and so an identical content hash — is stored for every
// role it reaches.
type bodyCache struct {
	html, text string
	recipients []string
}

// Pass runs one incremental fetch-and-ingest pass over roles, in order.
//
// The returned error is non-nil only for pass-fatal conditions: context
// cancellation, or a request failing with wilma.ErrSessionExpired (the
// whole session is dead, so there is no point trying the remaining roles).
// Every other failure — a bad list response, a body fetch error, a store
// error — is role-fatal: it stops that role only, is recorded in its
// RoleResult.Err, and Pass moves on to the next role.
func Pass(ctx context.Context, src Source, sink Sink, roles []Role, opt Options) (Result, error) {
	res := Result{Started: opt.now()}
	bodies := map[int64]bodyCache{}
	reqs := 0
	throttle := func() {
		if reqs > 0 {
			opt.sleep(opt.Delay)
		}
		reqs++
	}

	for _, role := range roles {
		if err := ctx.Err(); err != nil {
			res.Finished = opt.now()
			return res, err
		}

		rr, err := pollRole(ctx, src, sink, role, opt, bodies, throttle)
		res.Roles = append(res.Roles, rr)
		if err != nil {
			res.Finished = opt.now()
			return res, err
		}
	}

	res.Finished = opt.now()
	return res, nil
}

// pollRole polls one role. The returned error is set only for a pass-fatal
// condition (context cancellation or wilma.ErrSessionExpired); anything
// else is recorded in rr.Err and reported as nil here, so Pass continues to
// the next role.
func pollRole(ctx context.Context, src Source, sink Sink, role Role, opt Options, bodies map[int64]bodyCache, throttle func()) (RoleResult, error) {
	rr := RoleResult{Role: role.Prefix, Name: role.Name}

	mark, haveMark, err := sink.HighWaterMark(role.Prefix)
	if err != nil {
		rr.Err = fmt.Errorf("reading high-water mark: %w", err)
		return rr, nil
	}
	rr.MarkBefore, rr.MarkAfter = mark, mark
	rr.Bootstrap = !haveMark

	throttle()
	msgs, err := src.List(role.Prefix, role.Name)
	if err != nil {
		rr.Err = fmt.Errorf("listing messages: %w", err)
		if errors.Is(err, wilma.ErrSessionExpired) {
			return rr, rr.Err
		}
		return rr, nil
	}
	rr.Listed = len(msgs)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })

	// Reaching this point means the role's session is alive and its list
	// was read successfully — worth recording even if everything after
	// this fails.
	if err := sink.MarkPolled(role.Prefix); err != nil {
		rr.Err = fmt.Errorf("recording poll time: %w", err)
	}

	var cutoff time.Time
	if !haveMark && opt.Bootstrap > 0 {
		cutoff = opt.now().Add(-opt.Bootstrap)
	}
	sel := selectNew(msgs, mark, haveMark, cutoff)
	rr.Selected = len(sel)

	if !haveMark && opt.FromNow {
		if len(msgs) > 0 {
			newest := msgs[len(msgs)-1].ID
			if err := sink.SetHighWaterMark(role.Prefix, newest); err != nil {
				rr.Err = fmt.Errorf("setting high-water mark: %w", err)
			} else {
				rr.MarkAfter = newest
			}
		}
		return rr, nil
	}

	if rr.Bootstrap && len(sel) > 0 {
		if opt.Bodies {
			opt.warn("role %s (%s) has no recorded high-water mark; bootstrapping %d messages from the last %s — fetching bodies marks them read in Wilma; use --from-now to start from the newest message instead",
				role.Prefix, role.Name, len(sel), opt.Bootstrap)
		} else {
			opt.log("role %s (%s) has no recorded high-water mark; bootstrapping %d messages from the last %s (metadata only)",
				role.Prefix, role.Name, len(sel), opt.Bootstrap)
		}
	} else if rr.Selected == 0 && rr.Bootstrap {
		opt.log("role %s (%s): nothing within the bootstrap window; leaving the high-water mark unset", role.Prefix, role.Name)
	}

	for i := range sel {
		if err := ctx.Err(); err != nil {
			rr.Err = err
			return rr, err
		}

		m := sel[i]
		if opt.Bodies {
			if b, ok := bodies[m.ID]; ok {
				m.BodyHTML, m.BodyText, m.Recipients = b.html, b.text, b.recipients
			} else {
				throttle()
				if err := src.FillBody(role.Prefix, &m); err != nil {
					rr.Err = fmt.Errorf("fetching body for message %d: %w", m.ID, err)
					if errors.Is(err, wilma.ErrSessionExpired) {
						return rr, rr.Err
					}
					break
				}
				bodies[m.ID] = bodyCache{html: m.BodyHTML, text: m.BodyText, recipients: m.Recipients}
			}
		}

		isNew, err := sink.IngestMessage(m)
		if err != nil {
			rr.Err = fmt.Errorf("storing message %d: %w", m.ID, err)
			break
		}
		rr.Ingested++
		if isNew {
			rr.New++
		}

		if err := sink.SetHighWaterMark(role.Prefix, m.ID); err != nil {
			rr.Err = fmt.Errorf("advancing high-water mark to %d: %w", m.ID, err)
			break
		}
		rr.MarkAfter = m.ID
	}

	return rr, nil
}

// selectNew picks the messages a pass should act on, given the DB's
// current state for a role. msgs must already be sorted ascending by ID.
//
// With a recorded mark, only ID matters: anything above the mark is new,
// full stop. Time never enters into it, which is what makes per-message
// mark advancement crash-safe — a resumed pass re-derives exactly the same
// selection from the mark alone.
//
// Without a mark (a role's first ever poll, including a newly added
// child), sync's rule of never filtering out an unparseable timestamp
// would be dangerous here: if Wilma's TimeStamp format ever changes, every
// message parses to the zero time and looks in-window, so the very first
// automated poll would body-fetch — and so mark read — the account's
// entire history. Instead the time window is converted to an ID floor:
// scan forward for the first message whose timestamp parses and falls
// inside the window, then take everything from that ID up. Because Wilma
// IDs are monotonic with time (the rest of this codebase already relies on
// that via sync's --after), a message with an unparseable timestamp is
// included exactly when its ID places it inside the window — its true
// chronological position — rather than by a guess about a format this
// code doesn't understand. If nothing parses (or nothing falls in the
// window), the selection is empty and the caller leaves the mark unset, so
// the next pass tries again from scratch rather than adopting a bad floor.
func selectNew(msgs []wilma.Message, mark int64, haveMark bool, cutoff time.Time) []wilma.Message {
	if haveMark {
		for i, m := range msgs {
			if m.ID > mark {
				return msgs[i:]
			}
		}
		return nil
	}
	if cutoff.IsZero() {
		return msgs
	}
	for i, m := range msgs {
		if !m.SentAt.IsZero() && !m.SentAt.Before(cutoff) {
			return msgs[i:]
		}
	}
	return nil
}
