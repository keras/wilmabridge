package poll

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"wilmabridge/internal/store"
	"wilmabridge/internal/wilma"
)

// fixedNow is the clock every test pins Options.Now to, so "30 days ago"
// etc. is deterministic regardless of when the test actually runs.
var fixedNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func daysAgo(d int) time.Time { return fixedNow.Add(-time.Duration(d) * 24 * time.Hour) }

// newMsg builds a minimal wilma.Message for tests. sentAt zero produces an
// unparseable message (SentAtRaw "garbage", SentAt zero), mirroring what
// parseTimestamp returns on a format it doesn't recognize.
func newMsg(id int64, sentAt time.Time) wilma.Message {
	raw := "garbage"
	if !sentAt.IsZero() {
		raw = sentAt.Format("2006-01-02 15:04:05")
	}
	return wilma.Message{
		ID:        id,
		Subject:   fmt.Sprintf("Subject %d", id),
		SentAt:    sentAt,
		SentAtRaw: raw,
		URL:       fmt.Sprintf("https://espoo.inschool.fi/!x/messages/%d", id),
	}
}

// fakeSource is an in-memory Source. lists holds, per role prefix, the
// full folder Wilma would return (List always returns everything --
// filtering is Pass's job, not the source's, matching the real API).
type fakeSource struct {
	lists   map[string][]wilma.Message
	listErr map[string]error
	bodyErr map[int64]error

	listCalls int
	bodyCalls int
	bodyIDs   []int64

	// onFillBody, if set, runs after a successful fill for the given
	// message ID -- used to inject context cancellation mid-role.
	onFillBody func(id int64)
}

func (f *fakeSource) List(prefix, child string) ([]wilma.Message, error) {
	f.listCalls++
	if err, ok := f.listErr[prefix]; ok {
		return nil, err
	}
	src := f.lists[prefix]
	out := make([]wilma.Message, len(src))
	for i, m := range src {
		m.Role = prefix
		m.Child = child
		out[i] = m
	}
	return out, nil
}

func (f *fakeSource) FillBody(prefix string, m *wilma.Message) error {
	f.bodyCalls++
	f.bodyIDs = append(f.bodyIDs, m.ID)
	if err, ok := f.bodyErr[m.ID]; ok {
		return err
	}
	m.BodyHTML = fmt.Sprintf("<p>body %d</p>", m.ID)
	m.BodyText = fmt.Sprintf("body %d", m.ID)
	if f.onFillBody != nil {
		f.onFillBody(m.ID)
	}
	return nil
}

func newFakeSource() *fakeSource {
	return &fakeSource{lists: map[string][]wilma.Message{}, listErr: map[string]error{}, bodyErr: map[int64]error{}}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "wilma.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testOptions() Options {
	return Options{
		Bootstrap: 720 * time.Hour,
		Bodies:    true,
		Now:       func() time.Time { return fixedNow },
		Sleep:     func(time.Duration) {},
	}
}

// --- selectNew: pure windowing logic ---------------------------------

func TestSelectNew_Table(t *testing.T) {
	asc := []wilma.Message{newMsg(50, daysAgo(60)), newMsg(100, daysAgo(40)), newMsg(150, daysAgo(10)), newMsg(200, daysAgo(1))}
	cutoff := daysAgo(30)

	cases := []struct {
		name     string
		msgs     []wilma.Message
		mark     int64
		haveMark bool
		cutoff   time.Time
		wantIDs  []int64
	}{
		{"mark set: only ID matters", asc, 100, true, time.Time{}, []int64{150, 200}},
		{"mark set above everything: nothing selected", asc, 200, true, time.Time{}, nil},
		{"no mark, no cutoff: everything", asc, 0, false, time.Time{}, []int64{50, 100, 150, 200}},
		{"no mark, cutoff: ID floor at first in-window message", asc, 0, false, cutoff, []int64{150, 200}},
		{"no mark, cutoff: nothing in window", asc, 0, false, fixedNow.Add(time.Hour), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectNew(tc.msgs, tc.mark, tc.haveMark, tc.cutoff)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d messages, want %d (%v)", len(got), len(tc.wantIDs), got)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id {
					t.Errorf("got[%d].ID = %d, want %d", i, got[i].ID, id)
				}
			}
		})
	}
}

// --- Pass: end-to-end behavior over a real store ----------------------

func TestPass_BootstrapWindowOnEmptyDB(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{
		newMsg(1, daysAgo(40)), newMsg(2, daysAgo(35)), // outside the 30-day window
		newMsg(3, daysAgo(10)), newMsg(4, daysAgo(5)), newMsg(5, daysAgo(1)), // inside it
	}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(res.Roles) != 1 || res.Roles[0].Err != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	rr := res.Roles[0]
	if rr.Selected != 3 || rr.Ingested != 3 || rr.New != 3 {
		t.Errorf("rr = %+v, want Selected=Ingested=New=3", rr)
	}
	if rr.MarkAfter != 5 {
		t.Errorf("MarkAfter = %d, want 5", rr.MarkAfter)
	}
	if src.bodyCalls != 3 {
		t.Errorf("bodyCalls = %d, want 3 (the 2 out-of-window messages must never be fetched)", src.bodyCalls)
	}
	for _, id := range src.bodyIDs {
		if id == 1 || id == 2 {
			t.Errorf("message %d is outside the bootstrap window but its body was fetched (marks it read in Wilma)", id)
		}
	}
}

func TestPass_SecondPassFetchesOnlyNewerIDs(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(10)), newMsg(2, daysAgo(5)), newMsg(3, daysAgo(1))}

	if _, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Wilma always returns the whole folder -- simulate 2 new messages
	// arriving by extending the same list, not replacing it.
	src.lists[role] = append(src.lists[role], newMsg(4, daysAgo(0)), newMsg(5, fixedNow))
	src.listCalls, src.bodyCalls, src.bodyIDs = 0, 0, nil

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if src.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", src.listCalls)
	}
	if src.bodyCalls != 2 {
		t.Errorf("bodyCalls = %d, want 2 (only the new messages)", src.bodyCalls)
	}
	rr := res.Roles[0]
	if rr.Ingested != 2 || rr.MarkAfter != 5 {
		t.Errorf("rr = %+v, want Ingested=2 MarkAfter=5", rr)
	}
}

func TestPass_ThirdPassWithNothingNewIsANoOp(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(10)), newMsg(2, daysAgo(1))}

	if _, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	src.listCalls, src.bodyCalls = 0, 0

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if src.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", src.listCalls)
	}
	if src.bodyCalls != 0 {
		t.Errorf("bodyCalls = %d, want 0", src.bodyCalls)
	}
	rr := res.Roles[0]
	if rr.Ingested != 0 || rr.MarkAfter != 2 || rr.MarkBefore != 2 {
		t.Errorf("rr = %+v, want a pure no-op", rr)
	}
	if !res.Quiet() {
		t.Error("Result.Quiet() = false, want true for a clean no-op pass")
	}
}

func TestPass_NewRoleBootstrapsIndependently(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	roleA, roleB := "/!111", "/!222"
	src.lists[roleA] = []wilma.Message{newMsg(1, daysAgo(10))}

	if _, err := Pass(context.Background(), src, sink, []Role{{Prefix: roleA, Name: "Ella"}}, testOptions()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Role B (a newly added child) appears on the second pass.
	src.lists[roleA] = append(src.lists[roleA], newMsg(2, daysAgo(0)))
	src.lists[roleB] = []wilma.Message{newMsg(100, daysAgo(45)), newMsg(101, daysAgo(5))}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: roleA, Name: "Ella"}, {Prefix: roleB, Name: "Jooa"}}, testOptions())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(res.Roles) != 2 {
		t.Fatalf("got %d role results, want 2", len(res.Roles))
	}
	a, b := res.Roles[0], res.Roles[1]
	if a.Bootstrap {
		t.Errorf("role A should be incremental (has a mark), got Bootstrap=true")
	}
	if a.Ingested != 1 || a.MarkAfter != 2 {
		t.Errorf("role A = %+v, want Ingested=1 MarkAfter=2", a)
	}
	if !b.Bootstrap {
		t.Errorf("role B should bootstrap (no prior mark), got Bootstrap=false")
	}
	if b.Selected != 1 || b.Ingested != 1 || b.MarkAfter != 101 {
		t.Errorf("role B = %+v, want only msg 101 (msg 100 is outside the 30-day window)", b)
	}
}

func TestPass_BodyErrorStopsRoleAtLastContiguousSuccess(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(4)), newMsg(2, daysAgo(3)), newMsg(3, daysAgo(2)), newMsg(4, daysAgo(1))}
	src.bodyErr[3] = errors.New("wilma: transient fetch failure")
	opt := testOptions()
	opt.Bootstrap = 0 // no cutoff: select everything by ID alone

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	rr := res.Roles[0]
	if rr.Err == nil {
		t.Fatal("expected a role error from the failing body fetch")
	}
	if rr.Ingested != 2 || rr.MarkAfter != 2 {
		t.Errorf("rr = %+v, want Ingested=2 MarkAfter=2 (stopped at the last contiguous success)", rr)
	}
	for _, id := range src.bodyIDs {
		if id == 4 {
			t.Error("message 4 was fetched despite message 3 failing before it -- should have stopped the role")
		}
	}

	pending, err := sink.PendingMessages(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("PendingMessages = %+v, want exactly messages 1 and 2 (message 3 must not be stored with an empty body)", pending)
	}
	for _, pm := range pending {
		if pm.WilmaID == 3 {
			t.Error("message 3 was stored despite its body fetch failing -- this permanently poisons it (empty body -> extract_state=skipped, and a later successful fetch would create a duplicate row)")
		}
	}

	// A later pass, once the transient failure clears, must pick up right
	// where it left off -- no gap, no re-fetch of 1 or 2.
	delete(src.bodyErr, 3)
	src.bodyIDs = nil
	res2, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt)
	if err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	rr2 := res2.Roles[0]
	if rr2.Err != nil {
		t.Fatalf("second pass role error: %v", rr2.Err)
	}
	if rr2.Ingested != 2 || rr2.MarkAfter != 4 {
		t.Errorf("rr2 = %+v, want Ingested=2 MarkAfter=4", rr2)
	}
}

func TestPass_UnparseableTimestampPlacedByID(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{
		newMsg(1, daysAgo(40)), // outside the window, excluded
		newMsg(2, daysAgo(10)), // inside the window: this is the floor
		newMsg(3, time.Time{}), // unparseable, but above the floor -> included
		newMsg(4, daysAgo(1)),  // inside the window
	}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	rr := res.Roles[0]
	if rr.Selected != 3 || rr.Ingested != 3 {
		t.Fatalf("rr = %+v, want Selected=Ingested=3 (messages 2,3,4)", rr)
	}
	for _, id := range src.bodyIDs {
		if id == 1 {
			t.Error("message 1 (below the floor) was fetched")
		}
	}
	found3 := false
	for _, id := range src.bodyIDs {
		if id == 3 {
			found3 = true
		}
	}
	if !found3 {
		t.Error("message 3 (unparseable timestamp, above the floor) was not fetched")
	}
}

func TestPass_DedupAcrossRolesFetchesBodyOnce(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	roleA, roleB := "/!111", "/!222"
	shared := newMsg(501, daysAgo(1))
	src.lists[roleA] = []wilma.Message{shared}
	src.lists[roleB] = []wilma.Message{shared}
	opt := testOptions()
	opt.Bootstrap = 0

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: roleA, Name: "Ella"}, {Prefix: roleB, Name: "Jooa"}}, opt)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if src.bodyCalls != 1 {
		t.Errorf("bodyCalls = %d, want 1 (the same message, shared by two roles, must be fetched once)", src.bodyCalls)
	}
	listed, selected, ingested, newCount, failed := res.Totals()
	if ingested != 2 {
		t.Errorf("Totals ingested = %d, want 2 (once per role)", ingested)
	}
	if newCount != 1 {
		t.Errorf("Totals new = %d, want 1 (one distinct message)", newCount)
	}
	if listed != 2 || selected != 2 || failed != 0 {
		t.Errorf("Totals = listed=%d selected=%d failed=%d, want 2/2/0", listed, selected, failed)
	}
}

func TestPass_SessionExpiredAbortsPass(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	roleA, roleB := "/!111", "/!222"
	src.listErr[roleA] = fmt.Errorf("wilma: some error: %w", wilma.ErrSessionExpired)
	src.lists[roleB] = []wilma.Message{newMsg(1, daysAgo(1))}

	_, err := Pass(context.Background(), src, sink, []Role{{Prefix: roleA, Name: "Ella"}, {Prefix: roleB, Name: "Jooa"}}, testOptions())
	if !errors.Is(err, wilma.ErrSessionExpired) {
		t.Fatalf("Pass error = %v, want errors.Is(err, wilma.ErrSessionExpired)", err)
	}
	if _, ok, err := sink.HighWaterMark(roleB); err != nil || ok {
		t.Errorf("role B should never have been reached: ok=%v err=%v", ok, err)
	}
}

func TestPass_ListErrorIsRoleFatalNotPassFatal(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	roleA, roleB := "/!111", "/!222"
	src.listErr[roleA] = errors.New("wilma: 500 internal server error")
	src.lists[roleB] = []wilma.Message{newMsg(1, daysAgo(1))}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: roleA, Name: "Ella"}, {Prefix: roleB, Name: "Jooa"}}, testOptions())
	if err != nil {
		t.Fatalf("Pass returned a pass-fatal error for a non-expiry list failure: %v", err)
	}
	if res.Err() == nil {
		t.Error("Result.Err() = nil, want role A's failure reported")
	}
	if _, ok, _ := sink.HighWaterMark(roleA); ok {
		t.Error("role A should have no mark recorded after a list failure")
	}
	if res.Roles[1].Ingested != 1 {
		t.Errorf("role B = %+v, should still have been polled normally", res.Roles[1])
	}
}

func TestPass_BodiesFalseSkipsFetchAndAdvances(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(1)), newMsg(2, daysAgo(0))}
	opt := testOptions()
	opt.Bodies = false

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if src.bodyCalls != 0 {
		t.Errorf("bodyCalls = %d, want 0", src.bodyCalls)
	}
	if res.Roles[0].MarkAfter != 2 {
		t.Errorf("MarkAfter = %d, want 2 (mark still advances even without bodies)", res.Roles[0].MarkAfter)
	}
	pending, err := sink.PendingMessages(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingMessages = %+v, want empty (body-less messages are stored extract_state=skipped)", pending)
	}
}

func TestPass_FromNowSetsMarkWithoutFetching(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(10, daysAgo(20)), newMsg(20, daysAgo(10)), newMsg(30, daysAgo(1))}
	opt := testOptions()
	opt.FromNow = true

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if src.bodyCalls != 0 {
		t.Errorf("bodyCalls = %d, want 0", src.bodyCalls)
	}
	rr := res.Roles[0]
	if rr.Ingested != 0 || rr.MarkAfter != 30 {
		t.Errorf("rr = %+v, want Ingested=0 MarkAfter=30", rr)
	}
}

func TestPass_ContextCancelledMidRole(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(2)), newMsg(2, daysAgo(1))}

	ctx, cancel := context.WithCancel(context.Background())
	src.onFillBody = func(id int64) {
		if id == 1 {
			cancel()
		}
	}

	res, err := Pass(ctx, src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pass error = %v, want context.Canceled", err)
	}
	if len(res.Roles) != 1 || res.Roles[0].MarkAfter != 1 {
		t.Fatalf("res = %+v, want message 1 stored and mark at 1", res.Roles)
	}
	pending, _ := sink.PendingMessages(10)
	if len(pending) != 1 {
		t.Errorf("PendingMessages = %+v, want only message 1 (2 must not be stored after cancellation)", pending)
	}
}

func TestPass_HonoursMarksWrittenByIngest(t *testing.T) {
	sink := openTestStore(t)
	// Simulate marks left behind by the existing `sync | ingest` pipeline
	// (cmd/wilmabridge's cmdIngest calls exactly this).
	role := "/!111"
	if err := sink.SetHighWaterMark(role, 900); err != nil {
		t.Fatal(err)
	}

	src := newFakeSource()
	src.lists[role] = []wilma.Message{newMsg(850, daysAgo(5)), newMsg(900, daysAgo(3)), newMsg(950, daysAgo(1))}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	rr := res.Roles[0]
	if rr.Bootstrap {
		t.Error("role with a pre-existing mark should not be treated as a bootstrap")
	}
	if rr.Selected != 1 || rr.Ingested != 1 || rr.MarkAfter != 950 {
		t.Errorf("rr = %+v, want only message 950 fetched", rr)
	}
}

func TestPass_NewMessageFiresOnceForNewMessagesOnly(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(2)), newMsg(2, daysAgo(1))}
	opt := testOptions()
	var got []int64
	opt.NewMessage = func(m wilma.Message) error {
		got = append(got, m.ID)
		return nil
	}

	if _, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got = %v, want [1 2] emitted on the bootstrap pass", got)
	}

	// A second pass with nothing new must not re-emit.
	got = nil
	if _, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want nothing re-emitted on a no-op pass", got)
	}
}

func TestPass_NewMessageFiresOnceAcrossSharedRoles(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	roleA, roleB := "/!111", "/!222"
	shared := newMsg(501, daysAgo(1))
	src.lists[roleA] = []wilma.Message{shared}
	src.lists[roleB] = []wilma.Message{shared}
	opt := testOptions()
	opt.Bootstrap = 0
	var got []int64
	opt.NewMessage = func(m wilma.Message) error {
		got = append(got, m.ID)
		return nil
	}

	if _, err := Pass(context.Background(), src, sink, []Role{{Prefix: roleA, Name: "Ella"}, {Prefix: roleB, Name: "Jooa"}}, opt); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(got) != 1 || got[0] != 501 {
		t.Errorf("got = %v, want [501] emitted once despite being ingested for two roles", got)
	}
}

func TestPass_NewMessageErrorIsRoleFatal(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(2)), newMsg(2, daysAgo(1))}
	opt := testOptions()
	opt.Bootstrap = 0
	wantErr := errors.New("broken pipe")
	opt.NewMessage = func(m wilma.Message) error {
		if m.ID == 2 {
			return wantErr
		}
		return nil
	}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, opt)
	if err != nil {
		t.Fatalf("Pass returned a pass-fatal error for a non-expiry emit failure: %v", err)
	}
	rr := res.Roles[0]
	if rr.Err == nil || !errors.Is(rr.Err, wantErr) {
		t.Fatalf("rr.Err = %v, want it to wrap %v", rr.Err, wantErr)
	}
	if rr.Ingested != 2 || rr.MarkAfter != 1 {
		t.Errorf("rr = %+v, want Ingested=2 MarkAfter=1 (message 2 was stored but its failed emit must stop the mark from advancing past it)", rr)
	}
}

func TestPass_NothingInBootstrapWindowLeavesMarkUnset(t *testing.T) {
	sink := openTestStore(t)
	src := newFakeSource()
	role := "/!111"
	src.lists[role] = []wilma.Message{newMsg(1, daysAgo(60)), newMsg(2, daysAgo(45))}

	res, err := Pass(context.Background(), src, sink, []Role{{Prefix: role, Name: "Ella"}}, testOptions())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	rr := res.Roles[0]
	if rr.Selected != 0 || rr.Ingested != 0 {
		t.Errorf("rr = %+v, want nothing selected", rr)
	}
	if _, ok, _ := sink.HighWaterMark(role); ok {
		t.Error("high-water mark should remain unset when nothing falls inside the bootstrap window")
	}
	if _, ok, err := sink.LastPolledAt(role); err != nil || !ok {
		t.Errorf("LastPolledAt: ok=%v err=%v, want the role still recorded as polled", ok, err)
	}
}
