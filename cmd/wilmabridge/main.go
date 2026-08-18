// Command wilmabridge is a small, read-only, stateless CLI that logs into
// Wilma (Visma InSchool) with a guardian account and prints messages for
// each child (role) as NDJSON on stdout, so other tools can consume the
// stream without knowing anything about Wilma.
//
// It talks to Wilma's own unofficial JSON endpoints (see internal/wilma) —
// there is no official public API for guardians. Credentials are read only
// from WILMA_USER / WILMA_PASSWORD; there are no flag equivalents so they
// never end up in shell history or process listings.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wilmabridge/internal/extract"
	"wilmabridge/internal/gemini"
	"wilmabridge/internal/poll"
	"wilmabridge/internal/store"
	"wilmabridge/internal/wilma"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		args = []string{"sync"}
	}
	cmd, rest := args[0], args[1:]
	// Support `wilmabridge --host ...` as shorthand for `wilmabridge sync --host ...`.
	if strings.HasPrefix(cmd, "-") {
		cmd, rest = "sync", args
	}

	switch cmd {
	case "sync":
		return cmdSync(rest)
	case "poll":
		return cmdPoll(rest)
	case "roles":
		return cmdRoles(rest)
	case "probe":
		return cmdProbe(rest)
	case "extract":
		return cmdExtract(rest)
	case "reextract":
		return cmdReextract(rest)
	case "ingest":
		return cmdIngest(rest)
	case "review":
		return cmdReview(rest)
	case "db":
		return cmdDB(rest)
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "wilmabridge: unknown command %q\n\n", cmd)
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `wilmabridge — read-only Wilma message sync (NDJSON to stdout)

Usage:
  wilmabridge sync    [flags]   fetch messages, print NDJSON (default command)
  wilmabridge poll    [flags]   incrementally fetch new messages into -db and exit
                                 (--interval keeps polling); no extraction
  wilmabridge roles   [flags]   discover and print guardian roles (one per child)
  wilmabridge probe   [flags]   log in and dump raw endpoint responses (debugging)
  wilmabridge extract [flags]   read sync's NDJSON on stdin, print extracted dated
                                 events as NDJSON on stdout. Stateless (no -db):
                                 nothing is written anywhere. With -db: pulls from
                                 the persistent pending queue instead of stdin, and
                                 saves results, in addition to still printing them.
                                 -interval keeps draining the queue on a timer.
  wilmabridge reextract [flags] re-run extraction over already-extracted -db
                                 messages (e.g. a stronger -model, or after an
                                 extract_ver bump). Latest run wins; nothing
                                 already stored is deleted. -dry-run previews.
  wilmabridge ingest  [flags]   read sync's NDJSON on stdin, store it in -db
                                 (dedup across children; queue for extraction)
  wilmabridge review  [flags]   print events flagged needs_review from -db
  wilmabridge db last-id [flags]     print the stored per-role high-water mark(s)
  wilmabridge db set-last-id [flags] <id>  manually advance a role's high-water mark
  wilmabridge db runs [flags]        print extraction run history (newest first)

Credentials (required, env only):
  WILMA_USER       Wilma username
  WILMA_PASSWORD   Wilma password
  GEMINI_API_KEY   Google AI Studio API key (extract only; see -api-key-env)

Stateless pipeline (no persistence):
  wilmabridge sync --since 168h | wilmabridge extract > events.ndjson

Persistent pipeline (dedup + a durable review queue):
  wilmabridge sync --since 168h | wilmabridge ingest --db wilma.db
  wilmabridge extract --db wilma.db
  wilmabridge review --db wilma.db

Polling pipeline (cron-friendly, keeps -db in sync with no bookkeeping):
  wilmabridge poll --db wilma.db
  wilmabridge extract --db wilma.db

Run 'wilmabridge sync -h' etc. for flags.
`)
}

// credentials reads WILMA_USER/WILMA_PASSWORD, exiting the process (code 2)
// with a clear message if either is missing.
func credentials() (user, pass string, ok bool) {
	user = os.Getenv("WILMA_USER")
	pass = os.Getenv("WILMA_PASSWORD")
	if user == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: WILMA_USER is not set")
		return "", "", false
	}
	if pass == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: WILMA_PASSWORD is not set")
		return "", "", false
	}
	return user, pass, true
}

func verboseLogger(enabled bool) func(string, ...any) {
	if !enabled {
		return nil
	}
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// parseChildren parses "!012345=Aino,!067890=Väinö" into a prefix->name map.
func parseChildren(s string) map[string]string {
	m := map[string]string{}
	if s == "" {
		return m
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return m
}

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func cmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	host := fs.String("host", os.Getenv("WILMA_HOST"), "Wilma host, e.g. espoo.inschool.fi (default: $WILMA_HOST)")
	since := fs.Duration("since", 168*time.Hour, "only messages newer than now minus this duration (0 disables)")
	after := fs.Int64("after", 0, "only messages with Id greater than this")
	folder := fs.String("folder", "", "folder to list: \"\" (inbox), all, outbox, archive, drafts")
	var roleFilter repeatedFlag
	fs.Var(&roleFilter, "role", "restrict to one role prefix (e.g. !012345); repeatable")
	children := fs.String("children", os.Getenv("WILMA_CHILDREN"), "role label map \"!id=Name,!id=Name\" (default: $WILMA_CHILDREN)")
	bodies := fs.Bool("bodies", true, "fetch full message bodies (false = metadata only)")
	delay := fs.Duration("delay", 300*time.Millisecond, "pause between HTTP requests")
	verbose := fs.Bool("v", false, "log requests to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *host == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --host or $WILMA_HOST is required")
		return 2
	}
	user, pass, ok := credentials()
	if !ok {
		return 2
	}

	log := verboseLogger(*verbose)
	client, err := wilma.NewClient(*host, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}

	roles, err := client.LoginAndDiscoverRoles(user, pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: login failed:", err)
		return 1
	}
	defer client.Logout()

	labels := parseChildren(*children)
	roleSet := map[string]bool{}
	for _, r := range roleFilter {
		roleSet[normalizeRolePrefix(r)] = true
	}

	var cutoff time.Time
	if *since > 0 {
		cutoff = time.Now().Add(-*since)
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	exitCode := 0
	for _, role := range roles {
		if len(roleSet) > 0 && !roleSet[normalizeRolePrefix(role.Prefix)] {
			continue
		}
		name := role.Name
		if l, ok := labels[role.Prefix]; ok && l != "" {
			name = l
		}

		list, err := client.ListFolder(role.Prefix, *folder)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: listing messages for role %q: %v\n", role.Prefix, err)
			exitCode = 1
			continue
		}

		msgs := wilma.ToMessages(role.Prefix, name, *host, list)
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID > msgs[j].ID })

		for _, msg := range msgs {
			if msg.ID <= *after {
				continue
			}
			if *since > 0 && !msg.SentAt.IsZero() && msg.SentAt.Before(cutoff) {
				continue
			}

			if *bodies {
				time.Sleep(*delay)
				if err := wilma.FillBody(client, role.Prefix, &msg); err != nil {
					fmt.Fprintf(os.Stderr, "wilmabridge: fetching body for message %d: %v\n", msg.ID, err)
					msg.BodyError = err.Error()
				}
			}

			if err := enc.Encode(msg); err != nil {
				fmt.Fprintln(os.Stderr, "wilmabridge: writing output:", err)
				return 1
			}
		}
		time.Sleep(*delay)
	}

	return exitCode
}

// wilmaSource adapts *wilma.Client to poll.Source. It exists only because
// ListFolder returns an unexported slice type, so poll cannot name it in an
// interface the way cmdSync's inline loop does.
type wilmaSource struct {
	c    *wilma.Client
	host string
}

func (s wilmaSource) List(prefix, child string) ([]wilma.Message, error) {
	list, err := s.c.ListFolder(prefix, "")
	if err != nil {
		return nil, err
	}
	return wilma.ToMessages(prefix, child, s.host, list), nil
}

func (s wilmaSource) FillBody(prefix string, m *wilma.Message) error {
	return wilma.FillBody(s.c, prefix, m)
}

// buildPollRoles narrows the account's discovered roles to the ones poll
// should act on and attaches each one's display label, exactly as cmdSync
// does inline for its own loop.
func buildPollRoles(roles []wilma.Role, labels map[string]string, roleSet map[string]bool) []poll.Role {
	var out []poll.Role
	for _, r := range roles {
		if len(roleSet) > 0 && !roleSet[normalizeRolePrefix(r.Prefix)] {
			continue
		}
		name := r.Name
		if l, ok := labels[r.Prefix]; ok && l != "" {
			name = l
		}
		out = append(out, poll.Role{Prefix: r.Prefix, Name: name})
	}
	return out
}

func rolePrefixSet(roleFilter repeatedFlag) map[string]bool {
	set := map[string]bool{}
	for _, r := range roleFilter {
		set[normalizeRolePrefix(r)] = true
	}
	return set
}

// pollWarn prints an operational warning unconditionally, unlike -v detail:
// used for anything with a real side effect the user should see regardless
// of flags (currently: fetching bodies for a role's bootstrap window, which
// marks those messages read in Wilma).
func pollWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wilmabridge: poll: "+format+"\n", args...)
}

// reportPollResult prints a summary of one pass to stderr — nothing at all
// for a clean no-op pass (under cron, output means mail), one line per
// role that stored something or failed otherwise, plus a totals line when
// there's anything to report or -v is set. Returns the process exit code
// for this pass: 1 if any role failed, 0 otherwise.
func reportPollResult(res poll.Result, verbose bool) int {
	listed, _, ingested, newCount, failed := res.Totals()
	if !verbose && res.Quiet() {
		return 0
	}

	for _, rr := range res.Roles {
		name := rr.Name
		if name == "" {
			name = "(unnamed)"
		}
		if rr.Err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: poll: %s (%s): %v\n", rr.Role, name, rr.Err)
			continue
		}
		if !verbose && rr.Ingested == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "wilmabridge: poll: %s (%s): listed=%d new=%d ingested=%d mark %d -> %d\n",
			rr.Role, name, rr.Listed, rr.New, rr.Ingested, rr.MarkBefore, rr.MarkAfter)
	}
	fmt.Fprintf(os.Stderr, "wilmabridge: poll: %d roles, %d listed, %d stored (%d distinct new), %d errors, %s\n",
		len(res.Roles), listed, ingested, newCount, failed, res.Finished.Sub(res.Started).Round(10*time.Millisecond))

	if failed > 0 {
		return 1
	}
	return 0
}

// cmdPoll incrementally fetches new messages into -db and exits (or, with
// -interval, keeps doing so on a timer). Unlike sync, it always opens the
// database: each role's high-water mark there is exactly what makes it
// possible to fetch only what's newer than last time, without any
// external bookkeeping like cmdSync's --after requires. See -h and
// README.md's "poll: automatic incremental sync" section for the full
// algorithm and its trade-offs.
func cmdPoll(args []string) int {
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	host := fs.String("host", os.Getenv("WILMA_HOST"), "Wilma host, e.g. espoo.inschool.fi (default: $WILMA_HOST)")
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	bootstrap := fs.Duration("bootstrap", 720*time.Hour, "time window used only on a role's first ever poll (0 disables the cutoff)")
	fromNow := fs.Bool("from-now", false, "on a role's first ever poll, record the newest message id and fetch nothing")
	interval := fs.Duration("interval", 0, "if >0, keep polling on this interval instead of exiting after one pass (minimum 1m)")
	var roleFilter repeatedFlag
	fs.Var(&roleFilter, "role", "restrict to one role prefix (e.g. !012345); repeatable")
	children := fs.String("children", os.Getenv("WILMA_CHILDREN"), "role label map \"!id=Name,!id=Name\" (default: $WILMA_CHILDREN)")
	bodies := fs.Bool("bodies", true, "fetch full message bodies (false = metadata only; see README's poll section)")
	delay := fs.Duration("delay", 300*time.Millisecond, "pause between HTTP requests")
	verbose := fs.Bool("v", false, "log requests and per-role/per-message detail to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *host == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --host or $WILMA_HOST is required")
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}
	if *bootstrap < 0 {
		fmt.Fprintln(os.Stderr, "wilmabridge: --bootstrap must not be negative")
		return 2
	}
	if *interval != 0 && *interval < time.Minute {
		fmt.Fprintln(os.Stderr, "wilmabridge: --interval must be at least 1m (0 disables it, meaning one pass then exit)")
		return 2
	}
	user, pass, ok := credentials()
	if !ok {
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	if !*bodies {
		pollWarn("--bodies=false stores messages without a body; they are recorded extract_state=skipped and the high-water " +
			"mark still advances, so `extract` will never see them. To backfill bodies later: " +
			"wilmabridge sync --since 720h | wilmabridge ingest")
	}

	opt := poll.Options{
		Bootstrap: *bootstrap,
		FromNow:   *fromNow,
		Bodies:    *bodies,
		Delay:     *delay,
		Warn:      pollWarn,
	}
	if *verbose {
		opt.Log = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}

	roleLabels := parseChildren(*children)
	roleSet := rolePrefixSet(roleFilter)

	if *interval > 0 {
		return pollLoop(*host, user, pass, roleLabels, roleSet, st, opt, *interval, *verbose)
	}

	log := verboseLogger(*verbose)
	client, err := wilma.NewClient(*host, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	roles, err := client.LoginAndDiscoverRoles(user, pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: poll: login failed:", err)
		return 1
	}
	defer client.Logout()

	pollRoles := buildPollRoles(roles, roleLabels, roleSet)
	src := wilmaSource{c: client, host: *host}

	res, passErr := poll.Pass(context.Background(), src, st, pollRoles, opt)
	exitCode := reportPollResult(res, *verbose)
	if passErr != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: poll:", passErr)
		return 1
	}
	return exitCode
}

// pollLoop keeps polling on a timer until interrupted (SIGINT/SIGTERM) or a
// setup failure. It owns the Wilma session's whole lifecycle, including
// transparently logging back in when a pass reports wilma.ErrSessionExpired
// — the expected way for a long-running poller to find out its session
// died, since Wilma sessions time out well before a human would notice.
func pollLoop(host, user, pass string, roleLabels map[string]string, roleSet map[string]bool, st *store.Store, opt poll.Options, interval time.Duration, verbose bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := verboseLogger(verbose)

	var (
		client    *wilma.Client
		pollRoles []poll.Role
	)
	// login (re)establishes the session and refreshes the role list, so a
	// child added to the account mid-run gets picked up on the next login
	// rather than requiring a restart.
	login := func() error {
		c, err := wilma.NewClient(host, log)
		if err != nil {
			return err
		}
		roles, err := c.LoginAndDiscoverRoles(user, pass)
		if err != nil {
			return err
		}
		client = c
		pollRoles = buildPollRoles(roles, roleLabels, roleSet)
		return nil
	}

	if err := login(); err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: poll: login failed:", err)
		return 1
	}
	defer func() {
		if client != nil {
			client.Logout()
		}
	}()

	runPass := func() error {
		src := wilmaSource{c: client, host: host}
		res, err := poll.Pass(ctx, src, st, pollRoles, opt)
		reportPollResult(res, verbose)
		return err
	}

	failedLogins := 0
	for {
		if err := runPass(); err != nil {
			if ctx.Err() != nil {
				return 0 // requested shutdown mid-pass; not a failure
			}
			if errors.Is(err, wilma.ErrSessionExpired) {
				fmt.Fprintln(os.Stderr, "wilmabridge: poll: session expired, logging in again")
				client.Logout()
				if err := login(); err != nil {
					failedLogins++
					fmt.Fprintln(os.Stderr, "wilmabridge: poll: re-login failed:", err)
					if failedLogins >= 5 {
						fmt.Fprintln(os.Stderr, "wilmabridge: poll: giving up after 5 consecutive failed logins")
						return 1
					}
				} else {
					failedLogins = 0
					// One immediate retry with the fresh session, so a
					// mid-run expiry doesn't cost a whole --interval of
					// staleness. If it fails again too, wait for the next
					// tick rather than looping tightly.
					if err := runPass(); err != nil && ctx.Err() == nil {
						fmt.Fprintln(os.Stderr, "wilmabridge: poll:", err)
					} else if ctx.Err() != nil {
						return 0
					}
				}
			} else {
				// Pass only returns a non-ctx error for ErrSessionExpired,
				// but handle the general case defensively.
				fmt.Fprintln(os.Stderr, "wilmabridge: poll:", err)
			}
		}

		select {
		case <-ctx.Done():
			return 0
		case <-time.After(interval):
		}
	}
}

func cmdRoles(args []string) int {
	fs := flag.NewFlagSet("roles", flag.ContinueOnError)
	host := fs.String("host", os.Getenv("WILMA_HOST"), "Wilma host, e.g. espoo.inschool.fi (default: $WILMA_HOST)")
	verbose := fs.Bool("v", false, "log requests to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *host == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --host or $WILMA_HOST is required")
		return 2
	}
	user, pass, ok := credentials()
	if !ok {
		return 2
	}

	log := verboseLogger(*verbose)
	client, err := wilma.NewClient(*host, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	roles, err := client.LoginAndDiscoverRoles(user, pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: login failed:", err)
		return 1
	}
	defer client.Logout()

	for _, r := range roles {
		prefix := r.Prefix
		if prefix == "" {
			prefix = "(none)"
		}
		name := r.Name
		if name == "" {
			name = "(unnamed — set via --children/$WILMA_CHILDREN)"
		}
		fmt.Printf("%s\t%s\n", prefix, name)
	}
	return 0
}

func cmdProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	host := fs.String("host", os.Getenv("WILMA_HOST"), "Wilma host, e.g. espoo.inschool.fi (default: $WILMA_HOST)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *host == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --host or $WILMA_HOST is required")
		return 2
	}
	user, pass, ok := credentials()
	if !ok {
		return 2
	}

	log := verboseLogger(true)
	client, err := wilma.NewClient(*host, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	roles, err := client.LoginAndDiscoverRoles(user, pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: login failed:", err)
		return 1
	}
	defer client.Logout()

	fmt.Fprintf(os.Stderr, "\n== discovered roles ==\n")
	for _, r := range roles {
		fmt.Fprintf(os.Stderr, "  prefix=%q name=%q\n", r.Prefix, r.Name)
	}

	for _, path := range []string{"/api/v1/students", "/", "/messages/index_json"} {
		p := path
		if p != "/api/v1/students" && len(roles) > 0 {
			p = roles[0].Prefix + path
		}
		body, status, err := client.GetRaw(p)
		fmt.Fprintf(os.Stderr, "\n== GET %s (status=%d) ==\n", p, status)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		const max = 4000
		if len(body) > max {
			fmt.Fprintf(os.Stderr, "%s\n... (truncated, %d bytes total)\n", body[:max], len(body))
		} else {
			fmt.Fprintf(os.Stderr, "%s\n", body)
		}
	}
	return 0
}

// cmdExtract writes extract.Event-shaped NDJSON to stdout: one line per
// dated event found by Gemini, date-resolved and weekday-checked by Go.
//
// With no -db, it's stateless like sync — reads wilma.Message-shaped NDJSON
// from stdin, writes events to stdout, touches no file:
//
//	wilmabridge sync --since 168h | wilmabridge extract
//
// With -db, it instead pulls from that database's pending queue (populated
// by `ingest`), persists the results (events, an extractions audit row, and
// the message's extract_state/attempts) as one extraction run (see
// internal/store's Run type and "wilmabridge reextract"), and *also* still
// prints the events to stdout — so -v/piping keep working either way.
// -interval keeps draining the pending queue on a timer instead of exiting
// after one pass, the same idea as `poll -interval`.
func cmdExtract(args []string) int {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	apiKeyEnv := fs.String("api-key-env", "GEMINI_API_KEY", "name of the env var holding the Gemini API key")
	model := fs.String("model", "gemini-3.5-flash-lite", "Gemini model id")
	baseURL := fs.String("base-url", "", "override the Interactions API base URL (default: real endpoint)")
	delay := fs.Duration("delay", 200*time.Millisecond, "pause between API calls")
	maxRetries := fs.Int("max-retries", 3, "max attempts on 429/5xx before giving up on a message, within one call")
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB); enables persistent pending-queue mode instead of reading stdin")
	limit := fs.Int("limit", 20, "max pending messages to process this run (--db mode only)")
	maxAttempts := fs.Int("max-attempts", 5, "mark a message failed (stop retrying) after this many failed runs (--db mode only)")
	interval := fs.Duration("interval", 0, "if >0, keep draining the pending queue on this interval instead of exiting after one pass (minimum 1m; --db mode only)")
	note := fs.String("note", "", "free text recorded on this run's extraction_runs row (default: \"pending queue\"; --db mode only)")
	verbose := fs.Bool("v", false, "log request/response summaries to stderr (never the key)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *interval != 0 && *interval < time.Minute {
		fmt.Fprintln(os.Stderr, "wilmabridge: --interval must be at least 1m (0 disables it, meaning one pass then exit)")
		return 2
	}
	if *interval != 0 && *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --interval requires --db (stdin mode reads a finite stream, not an ongoing queue)")
		return 2
	}

	apiKey := os.Getenv(*apiKeyEnv)
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "wilmabridge: $%s is not set\n", *apiKeyEnv)
		return 2
	}

	log := verboseLogger(*verbose)
	opts := []gemini.Option{gemini.WithMaxRetries(*maxRetries), gemini.WithVerbose(log)}
	if *baseURL != "" {
		opts = append(opts, gemini.WithBaseURL(*baseURL))
	}
	client, err := gemini.NewClient(apiKey, *model, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}

	if *dbPath != "" {
		return extractFromDB(client, *model, *dbPath, *limit, *maxAttempts, *delay, *interval, *note, *verbose)
	}
	return extractFromStdin(client, *delay)
}

// extractFromStdin is the original, unchanged stateless mode.
func extractFromStdin(client *gemini.Client, delay time.Duration) int {
	in := bufio.NewScanner(os.Stdin)
	// Message bodies (body_html especially) can be large; grow past
	// bufio.Scanner's 64KB default rather than truncating a long message.
	in.Buffer(make([]byte, 0, 64*1024), 4<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	ctx := context.Background()
	exitCode := 0
	lineNo := 0
	callCount := 0
	for in.Scan() {
		lineNo++
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg wilma.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: stdin line %d: invalid JSON: %v\n", lineNo, err)
			exitCode = 1
			continue
		}
		if strings.TrimSpace(msg.BodyText) == "" {
			fmt.Fprintf(os.Stderr, "wilmabridge: message %d: no body text (fetched with sync --bodies=false?), skipping\n", msg.ID)
			continue
		}

		if callCount > 0 {
			time.Sleep(delay)
		}
		callCount++

		events, _, err := extract.ExtractMessage(ctx, client, msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: extracting message %d: %v\n", msg.ID, err)
			exitCode = 1
			continue
		}
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				fmt.Fprintln(os.Stderr, "wilmabridge: writing output:", err)
				return 1
			}
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: reading stdin:", err)
		return 1
	}

	return exitCode
}

// batchSelector picks the messages one extraction pass will work on. It's
// the only thing that differs between `extract --db` (the pending queue)
// and `reextract` (already-extracted messages eligible for another go).
type batchSelector func(*store.Store) ([]store.PendingMessage, error)

// extractOptions configures one extractPass/extractLoop.
type extractOptions struct {
	Model       string
	MaxAttempts int
	Delay       time.Duration
	Note        string // recorded on this pass's extraction_runs row
	Verbose     bool
}

// sleepCtx pauses for d, or returns early if ctx is cancelled first — used
// between HTTP calls so a shutdown doesn't have to wait out a full delay.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// extractPass runs one extraction run end to end: select a batch via sel,
// open a run, extract/save/mark each message, close the run, stream events
// to enc. Creates no run and prints nothing when the batch is empty, so a
// cron'd or --interval'd pass with nothing to do stays silent (poll's
// "under cron, output means mail" rule). processed counts messages
// successfully extracted and stored; exitCode follows this project's usual
// convention (0 clean, 1 any message-level failure).
func extractPass(ctx context.Context, st *store.Store, client *gemini.Client, sel batchSelector, opt extractOptions, enc *json.Encoder, out *bufio.Writer) (processed, exitCode int) {
	batch, err := sel(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 0, 1
	}
	if len(batch) == 0 {
		return 0, 0
	}

	note := opt.Note
	if note == "" {
		note = "pending queue"
	}
	runID, err := st.StartRun(opt.Model, extract.ExtractVer, note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 0, 1
	}
	finish := func() {
		if err := st.FinishRun(runID); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: finishing run %d: %v\n", runID, err)
		}
		if err := out.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, "wilmabridge: writing output:", err)
			exitCode = 1
		}
	}

	consecutiveFailures := 0
	for i, pm := range batch {
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			sleepCtx(ctx, opt.Delay)
			if ctx.Err() != nil {
				break
			}
		}

		msg := wilma.Message{
			ID: pm.WilmaID, Subject: pm.Subject, BodyText: pm.BodyText,
			SentAt: pm.SentAt, SentAtRaw: pm.SentAtRaw, URL: pm.URL,
		}
		events, exchange, extractErr := extract.ExtractMessage(ctx, client, msg)

		if err := st.SaveExtraction(runID, pm.WilmaID, pm.ContentHash, opt.Model, exchange, extractErr); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: recording extraction audit for message %d: %v\n", pm.WilmaID, err)
			exitCode = 1
		}

		if extractErr != nil {
			if ctx.Err() != nil {
				// Cancelled mid-call: this was never a fair attempt, so
				// don't burn retry budget on it via MarkExtractFailed.
				break
			}
			fmt.Fprintf(os.Stderr, "wilmabridge: extracting message %d: %v\n", pm.WilmaID, extractErr)
			if err := st.MarkExtractFailed(pm.WilmaID, pm.ContentHash, extractErr.Error(), opt.MaxAttempts); err != nil {
				fmt.Fprintf(os.Stderr, "wilmabridge: marking message %d failed: %v\n", pm.WilmaID, err)
			}
			exitCode = 1
			consecutiveFailures++
			if consecutiveFailures >= 3 {
				// A dead API key or a Gemini outage would otherwise burn
				// every remaining message's retry budget in one pass, with
				// no requeue command today to undo it. Stop and let the
				// next tick (or the next cron invocation) try again.
				fmt.Fprintln(os.Stderr, "wilmabridge: extract: 3 consecutive failures, aborting this pass")
				break
			}
			continue
		}
		consecutiveFailures = 0

		if err := st.SaveEvents(runID, pm.WilmaID, pm.ContentHash, events); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: saving events for message %d: %v\n", pm.WilmaID, err)
			exitCode = 1
			continue
		}
		if err := st.MarkExtractDone(pm.WilmaID, pm.ContentHash, extract.ExtractVer); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: marking message %d done: %v\n", pm.WilmaID, err)
			exitCode = 1
			continue
		}
		processed++

		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				fmt.Fprintln(os.Stderr, "wilmabridge: writing output:", err)
				exitCode = 1
				finish()
				return processed, exitCode
			}
		}
	}

	finish()
	return processed, exitCode
}

// extractLoop repeats extractPass on a timer until SIGINT/SIGTERM. Simpler
// than poll's interval loop: there's no Wilma session to keep alive or
// re-establish, just Gemini calls and DB writes, so a failed pass just
// waits for the next tick.
func extractLoop(st *store.Store, client *gemini.Client, sel batchSelector, opt extractOptions, enc *json.Encoder, out *bufio.Writer, interval time.Duration) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		if ctx.Err() != nil {
			return 0
		}
		if _, exitCode := extractPass(ctx, st, client, sel, opt, enc, out); exitCode != 0 && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "wilmabridge: extract: pass failed, will retry next tick")
		}

		select {
		case <-ctx.Done():
			return 0
		case <-time.After(interval):
		}
	}
}

// extractFromDB pulls the pending queue from a SQLite database (populated
// by `ingest`), extracts each, and persists events + an extractions audit
// row + the message's new extract_state, while still streaming events to
// stdout exactly like the stdin mode does. interval>0 keeps doing this on a
// timer instead of exiting after one pass.
func extractFromDB(client *gemini.Client, model, dbPath string, limit, maxAttempts int, delay, interval time.Duration, note string, verbose bool) int {
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	opt := extractOptions{Model: model, MaxAttempts: maxAttempts, Delay: delay, Note: note, Verbose: verbose}
	sel := func(st *store.Store) ([]store.PendingMessage, error) { return st.PendingMessages(limit) }

	if interval > 0 {
		return extractLoop(st, client, sel, opt, enc, out, interval)
	}

	_, exitCode := extractPass(context.Background(), st, client, sel, opt, enc, out)
	return exitCode
}

// repeatedInt64Flag collects repeated -id flags into a slice.
type repeatedInt64Flag []int64

func (r *repeatedInt64Flag) String() string {
	parts := make([]string, len(*r))
	for i, id := range *r {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func (r *repeatedInt64Flag) Set(v string) error {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a valid wilma_id: %w", v, err)
	}
	*r = append(*r, id)
	return nil
}

// cmdReextract re-runs extraction over messages that already have a result
// — the escape hatch for "the prompt improved" or "this deserves a stronger
// model" that `extract` (pending-queue only) deliberately doesn't cover.
// Built on the same extractPass/batchSelector as extract --db; the only
// difference is which messages the selector returns. Nothing already stored
// is ever deleted or mutated: see internal/store's LatestEvents doc comment
// for how "latest run wins" is derived, not flagged.
func cmdReextract(args []string) int {
	fs := flag.NewFlagSet("reextract", flag.ContinueOnError)
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	apiKeyEnv := fs.String("api-key-env", "GEMINI_API_KEY", "name of the env var holding the Gemini API key")
	model := fs.String("model", "gemini-3.5-flash-lite", "Gemini model id")
	baseURL := fs.String("base-url", "", "override the Interactions API base URL (default: real endpoint)")
	delay := fs.Duration("delay", 200*time.Millisecond, "pause between API calls")
	maxRetries := fs.Int("max-retries", 3, "max attempts on 429/5xx before giving up on a message, within one call")
	maxAttempts := fs.Int("max-attempts", 5, "mark a message failed (stop retrying) after this many failed runs")
	limit := fs.Int("limit", 20, "max messages to process this run (a reextract costs real money; deliberately not unlimited)")
	force := fs.Bool("force", false, "ignore extract_ver; redo even messages already extracted at the current version")
	since := fs.Duration("since", 0, "only messages sent within this duration before now (0 = no lower bound)")
	var ids repeatedInt64Flag
	fs.Var(&ids, "id", "restrict to one wilma_id; repeatable")
	note := fs.String("note", "", "free text recorded on this run's extraction_runs row (default: auto-generated)")
	dryRun := fs.Bool("dry-run", false, "print the selected messages and exit; no Gemini calls, no run created")
	verbose := fs.Bool("v", false, "log request/response summaries to stderr (never the key)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	filter := store.ReextractFilter{ExtractVer: extract.ExtractVer, Force: *force, Limit: *limit, WilmaIDs: ids}
	if *since > 0 {
		filter.Since = time.Now().Add(-*since)
	}

	if *dryRun {
		candidates, err := st.ReextractMessages(filter)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wilmabridge:", err)
			return 1
		}
		for _, pm := range candidates {
			fmt.Printf("%d\t%s\t%s\n", pm.WilmaID, pm.SentAtRaw, pm.Subject)
		}
		fmt.Fprintf(os.Stderr, "wilmabridge: reextract: %d messages selected (dry run, no Gemini calls made)\n", len(candidates))
		return 0
	}

	apiKey := os.Getenv(*apiKeyEnv)
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "wilmabridge: $%s is not set\n", *apiKeyEnv)
		return 2
	}
	log := verboseLogger(*verbose)
	opts := []gemini.Option{gemini.WithMaxRetries(*maxRetries), gemini.WithVerbose(log)}
	if *baseURL != "" {
		opts = append(opts, gemini.WithBaseURL(*baseURL))
	}
	client, err := gemini.NewClient(apiKey, *model, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}

	runNote := *note
	if runNote == "" {
		runNote = fmt.Sprintf("reextract model=%s force=%v", *model, *force)
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	opt := extractOptions{Model: *model, MaxAttempts: *maxAttempts, Delay: *delay, Note: runNote, Verbose: *verbose}
	sel := func(st *store.Store) ([]store.PendingMessage, error) { return st.ReextractMessages(filter) }

	processed, exitCode := extractPass(context.Background(), st, client, sel, opt, enc, out)
	if processed == 0 && exitCode == 0 {
		fmt.Fprintln(os.Stderr, "wilmabridge: reextract: nothing eligible (try --force, or check --since/--id)")
	}
	return exitCode
}

// cmdIngest reads wilma.Message-shaped NDJSON from stdin (sync's output)
// and stores it in -db: deduplicated across children (the same Wilma
// message sent to both kids collapses to one row, see internal/store),
// queued for extraction, and with each role's high-water mark updated so a
// later `sync --after $(wilmabridge db last-id --role ...)` needs no
// external bookkeeping.
func cmdIngest(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	verbose := fs.Bool("v", false, "log each ingested message to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4<<20)

	exitCode := 0
	lineNo, total, newCount := 0, 0, 0
	highWater := map[string]int64{}
	for in.Scan() {
		lineNo++
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg wilma.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: stdin line %d: invalid JSON: %v\n", lineNo, err)
			exitCode = 1
			continue
		}

		isNew, err := st.IngestMessage(msg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wilmabridge:", err)
			exitCode = 1
			continue
		}
		total++
		if isNew {
			newCount++
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "ingested wilma_id=%d child=%q new=%v\n", msg.ID, msg.Child, isNew)
		}
		if msg.Role != "" && msg.ID > highWater[msg.Role] {
			highWater[msg.Role] = msg.ID
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge: reading stdin:", err)
		return 1
	}

	for role, id := range highWater {
		if err := st.SetHighWaterMark(role, id); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: recording high-water mark for %q: %v\n", role, err)
			exitCode = 1
		}
	}

	fmt.Fprintf(os.Stderr, "wilmabridge: ingested %d messages (%d new)\n", total, newCount)
	return exitCode
}

// cmdReview prints every event flagged needs_review, as NDJSON, annotated
// with which children it concerns.
func cmdReview(args []string) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	rows, err := st.NeedsReview()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "wilmabridge: writing output:", err)
			return 1
		}
	}
	return 0
}

// cmdDB dispatches "db" subcommands: last-id/set-last-id (sync/poll
// bookkeeping) and runs (extraction run history); the namespace exists so
// inspection commands like these have a home that isn't the top-level
// command list.
func cmdDB(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "wilmabridge: db: expected a subcommand (last-id, set-last-id, runs)")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "last-id":
		return cmdDBLastID(rest)
	case "set-last-id":
		return cmdDBSetLastID(rest)
	case "runs":
		return cmdDBRuns(rest)
	default:
		fmt.Fprintf(os.Stderr, "wilmabridge: db: unknown subcommand %q\n", sub)
		return 2
	}
}

// cmdDBRuns prints extraction run history as NDJSON — the only other way to
// see it is querying extraction_runs directly with sqlite3. Newest first.
func cmdDBRuns(args []string) int {
	fs := flag.NewFlagSet("db runs", flag.ContinueOnError)
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	limit := fs.Int("limit", 20, "max runs to print, newest first")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	runs, err := st.Runs(*limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	for _, r := range runs {
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "wilmabridge: writing output:", err)
			return 1
		}
	}
	return 0
}

func cmdDBLastID(args []string) int {
	fs := flag.NewFlagSet("db last-id", flag.ContinueOnError)
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	role := fs.String("role", "", "restrict to one role prefix; default prints every known role")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	if *role != "" {
		id, ok, err := st.HighWaterMark(normalizeRolePrefix(*role))
		if err != nil {
			fmt.Fprintln(os.Stderr, "wilmabridge:", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "wilmabridge: no high-water mark recorded for %q\n", *role)
			return 1
		}
		fmt.Println(id)
		return 0
	}

	all, err := st.AllHighWaterMarks()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	polled, err := st.AllLastPolledAt()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	roles := make([]string, 0, len(all))
	for role := range all {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		lastPolled := "-"
		if t, ok := polled[role]; ok {
			lastPolled = t.Format(time.RFC3339)
		}
		fmt.Printf("%s\t%d\t%s\n", role, all[role], lastPolled)
	}
	return 0
}

// cmdDBSetLastID manually advances a role's high-water mark. It exists as
// the escape hatch for a role poll has stopped polling because one message
// keeps failing to fetch (see poll's "why a body-fetch error stops the
// role" design note): pointing the mark past the poison message lets poll
// resume without it. SetHighWaterMark never moves a mark backward, so this
// cannot be used to accidentally roll one back and re-fetch history.
func cmdDBSetLastID(args []string) int {
	fs := flag.NewFlagSet("db set-last-id", flag.ContinueOnError)
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB)")
	role := fs.String("role", "", "role prefix to set the mark for (e.g. !012345); required")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --db or $WILMA_DB is required")
		return 2
	}
	if *role == "" {
		fmt.Fprintln(os.Stderr, "wilmabridge: --role is required")
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "wilmabridge: db set-last-id: expected exactly one argument, the new id")
		return 2
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wilmabridge: db set-last-id: %q is not a valid id: %v\n", fs.Arg(0), err)
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	normalized := normalizeRolePrefix(*role)
	before, hadMark, err := st.HighWaterMark(normalized)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	if err := st.SetHighWaterMark(normalized, id); err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	after, _, err := st.HighWaterMark(normalized)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	if hadMark && after <= before {
		fmt.Fprintf(os.Stderr, "wilmabridge: db set-last-id: %d does not advance %s's mark (still %d) — SetHighWaterMark never moves it backward\n", id, normalized, after)
		return 1
	}
	fmt.Printf("%s: %d -> %d\n", normalized, before, after)
	return 0
}

func normalizeRolePrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return s
}
