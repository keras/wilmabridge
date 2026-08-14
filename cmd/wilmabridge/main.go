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
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"wilmabridge/internal/extract"
	"wilmabridge/internal/gemini"
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
	case "roles":
		return cmdRoles(rest)
	case "probe":
		return cmdProbe(rest)
	case "extract":
		return cmdExtract(rest)
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
  wilmabridge roles   [flags]   discover and print guardian roles (one per child)
  wilmabridge probe   [flags]   log in and dump raw endpoint responses (debugging)
  wilmabridge extract [flags]   read sync's NDJSON on stdin, print extracted dated
                                 events as NDJSON on stdout. Stateless (no -db):
                                 nothing is written anywhere. With -db: pulls from
                                 the persistent pending queue instead of stdin, and
                                 saves results, in addition to still printing them.
  wilmabridge ingest  [flags]   read sync's NDJSON on stdin, store it in -db
                                 (dedup across children; queue for extraction)
  wilmabridge review  [flags]   print events flagged needs_review from -db
  wilmabridge db last-id [flags]  print the stored per-role high-water mark(s)

Credentials (required, env only):
  WILMA_USER       Wilma username
  WILMA_PASSWORD   Wilma password
  AISTUDIO_KEY     Google AI Studio API key (extract only; see -api-key-env)

Stateless pipeline (no persistence):
  wilmabridge sync --since 168h | wilmabridge extract > events.ndjson

Persistent pipeline (dedup + a durable review queue):
  wilmabridge sync --since 168h | wilmabridge ingest --db wilma.db
  wilmabridge extract --db wilma.db
  wilmabridge review --db wilma.db

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
// the message's extract_state/attempts), and *also* still prints the
// events to stdout — so -v/piping keep working either way.
func cmdExtract(args []string) int {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	apiKeyEnv := fs.String("api-key-env", "AISTUDIO_KEY", "name of the env var holding the Gemini API key")
	model := fs.String("model", "gemini-3.5-flash-lite", "Gemini model id")
	baseURL := fs.String("base-url", "", "override the Interactions API base URL (default: real endpoint)")
	delay := fs.Duration("delay", 200*time.Millisecond, "pause between API calls")
	maxRetries := fs.Int("max-retries", 3, "max attempts on 429/5xx before giving up on a message, within one call")
	dbPath := fs.String("db", os.Getenv("WILMA_DB"), "SQLite database path (default: $WILMA_DB); enables persistent pending-queue mode instead of reading stdin")
	limit := fs.Int("limit", 20, "max pending messages to process this run (--db mode only)")
	maxAttempts := fs.Int("max-attempts", 5, "mark a message failed (stop retrying) after this many failed runs (--db mode only)")
	verbose := fs.Bool("v", false, "log request/response summaries to stderr (never the key)")
	if err := fs.Parse(args); err != nil {
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
		return extractFromDB(client, *model, *dbPath, *limit, *maxAttempts, *delay)
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

// extractFromDB pulls the pending queue from a SQLite database (populated
// by `ingest`), extracts each, and persists events + an extractions audit
// row + the message's new extract_state, while still streaming events to
// stdout exactly like the stdin mode does.
func extractFromDB(client *gemini.Client, model, dbPath string, limit, maxAttempts int, delay time.Duration) int {
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}
	defer st.Close()

	pending, err := st.PendingMessages(limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wilmabridge:", err)
		return 1
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	ctx := context.Background()
	exitCode := 0
	for i, pm := range pending {
		if i > 0 {
			time.Sleep(delay)
		}

		msg := wilma.Message{
			ID: pm.WilmaID, Subject: pm.Subject, BodyText: pm.BodyText,
			SentAt: pm.SentAt, SentAtRaw: pm.SentAtRaw, URL: pm.URL,
		}
		events, exchange, extractErr := extract.ExtractMessage(ctx, client, msg)

		if err := st.SaveExtraction(pm.WilmaID, pm.ContentHash, model, exchange, extractErr); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: recording extraction audit for message %d: %v\n", pm.WilmaID, err)
			exitCode = 1
		}

		if extractErr != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: extracting message %d: %v\n", pm.WilmaID, extractErr)
			if err := st.MarkExtractFailed(pm.WilmaID, pm.ContentHash, extractErr.Error(), maxAttempts); err != nil {
				fmt.Fprintf(os.Stderr, "wilmabridge: marking message %d failed: %v\n", pm.WilmaID, err)
			}
			exitCode = 1
			continue
		}

		if err := st.SaveEvents(pm.WilmaID, pm.ContentHash, events); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: saving events for message %d: %v\n", pm.WilmaID, err)
			exitCode = 1
			continue
		}
		if err := st.MarkExtractDone(pm.WilmaID, pm.ContentHash, extract.ExtractVer); err != nil {
			fmt.Fprintf(os.Stderr, "wilmabridge: marking message %d done: %v\n", pm.WilmaID, err)
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

// cmdDB dispatches "db" subcommands. Currently just last-id; the namespace
// exists so future inspection commands (stats, agenda, ...) have a home
// that isn't the top-level command list.
func cmdDB(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "wilmabridge: db: expected a subcommand (last-id)")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "last-id":
		return cmdDBLastID(rest)
	default:
		fmt.Fprintf(os.Stderr, "wilmabridge: db: unknown subcommand %q\n", sub)
		return 2
	}
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
	for role, id := range all {
		fmt.Printf("%s\t%d\n", role, id)
	}
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
