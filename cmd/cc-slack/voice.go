package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	spawnllm "github.com/yasyf/spawnllm/go"
)

const (
	voicePath        = "/voice"
	voiceJevBudget   = 600 * time.Millisecond
	voiceJevLate     = 5 * time.Second
	voicePostLimit   = 10 * time.Second
	voiceCutAt       = 0.65
	voiceOpenerCutAt = 0.9
	voiceRepeatAt    = 0.8
	voiceRepeatFor   = 10 * time.Minute
	voiceMaxUnits    = 16
	voiceLabelWord   = 6
	voiceOwnWords    = 8
	voiceBodyBytes   = 64 << 10
	voiceIdle        = time.Minute
	voiceLogName     = "voice.jsonl"
)

var (
	voicePRLink     = regexp.MustCompile(`<(https://github\.com/[\w.-]+/[\w.-]+/pull/(\d+))(?:\|([^<>]*))?>`)
	barePRLink      = regexp.MustCompile(`(^|[\s(])(https://github\.com/[\w.-]+/[\w.-]+/pull/(\d+))\b`)
	bareSlackLink   = regexp.MustCompile(`(^|[\s(])(https://[\w-]+\.slack\.com/archives/[A-Z0-9]+/p(\d{10})\d{6}[^\s<>]*)`)
	spaceBeforeStop = regexp.MustCompile(`(>) +([.,;])`)
	unresolvedRef   = regexp.MustCompile(`\{(?:pr|card|order|record|answer|started|at|release):[^{}]*\}`)
	deadTarget      = regexp.MustCompile(`<(?:(?:https?://)?(?:localhost|127\.0\.0\.1|0\.0\.0\.0)(?:[:/][^<>|]*)?|file://[^<>|]*|/[^<>|]*|\s*)(?:\|[^<>]*)?>`)
	secretShapes    = regexp.MustCompile(`xox[abpors]-[\w-]{10,}|xapp-[\w-]{10,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_\w{40,}|sk-ant-[\w-]{20,}|sk-[A-Za-z0-9]{40,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|eyJ[\w-]{20,}\.eyJ[\w-]{20,}\.[\w-]{10,}`)
	listLine        = regexp.MustCompile(`^\s*(?:[-*•◦]|\d+[.)])\s`)
	headingLine     = regexp.MustCompile(`^\s*\*[^*\n]+\*\s*$`)
)

type voiceRequest struct {
	Lane      string `json:"lane"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Text      string `json:"text"`
	Rules     string `json:"rules"`
	Asked     string `json:"asked,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

type voiceOutcome struct {
	ID        string             `json:"id"`
	At        time.Time          `json:"at"`
	Lane      string             `json:"lane"`
	ChannelID string             `json:"channel_id"`
	ThreadTS  string             `json:"thread_ts,omitempty"`
	Before    string             `json:"before"`
	Formatted string             `json:"formatted"`
	After     string             `json:"after"`
	Repeat    float64            `json:"repeat,omitempty"`
	Scores    map[string]float64 `json:"scores,omitempty"`
	Cut       []string           `json:"cut,omitempty"`
	Dropped   string             `json:"dropped,omitempty"`
	Jev       string             `json:"jev"`
	Edited    bool               `json:"edited,omitempty"`
	Permalink string             `json:"permalink,omitempty"`
	Error     string             `json:"error,omitempty"`
	DryRun    bool               `json:"dry_run,omitempty"`
	QueuedMS  int64              `json:"queued_ms"`
	FormatMS  int64              `json:"format_ms"`
	JevMS     int64              `json:"jev_ms"`
	PostMS    int64              `json:"post_ms"`
	TotalMS   int64              `json:"total_ms"`
}

type voiceUnit struct {
	text  string
	after string
}

type voiceEdits struct {
	cut    []string
	note   string
	repeat float64
	scores map[string]float64
}

type voiceJob struct {
	req      voiceRequest
	id       string
	received time.Time
}

type voiceDecide func(ctx context.Context, state any, questions map[string]spawnllm.Question, opts spawnllm.DecideOpts) (spawnllm.Decision, error)

type voiceSender struct {
	app    *app
	decide voiceDecide
	logTo  string
	log    *log.Logger

	mu     sync.Mutex
	queues map[string]chan voiceJob
	last   map[string]voiceLast
	rules  map[string]voiceRules
	write  sync.Mutex
}

type voiceRules struct {
	text string
	mod  time.Time
}

func newVoiceSender(a *app, decide voiceDecide) *voiceSender {
	return &voiceSender{
		app:    a,
		decide: decide,
		logTo:  filepath.Join(appPaths().StateDir(), voiceLogName),
		log:    log.New(os.Stderr, "["+binaryName+" voice] ", log.LstdFlags),
		queues: map[string]chan voiceJob{},
		last:   map[string]voiceLast{},
		rules:  map[string]voiceRules{},
	}
}

func (v *voiceSender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := callerWindow(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var req voiceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || strings.TrimSpace(req.Text) == "" || req.Rules == "" {
		http.Error(w, "voice needs channel_id, text and rules", http.StatusBadRequest)
		return
	}
	job := voiceJob{req: req, id: voiceID(), received: time.Now()}
	w.Header().Set("Content-Type", "application/json")
	if req.DryRun {
		_ = json.NewEncoder(w).Encode(v.run(r.Context(), job))
		return
	}
	v.enqueue(job)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": job.id})
}

func voiceID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (v *voiceSender) enqueue(job voiceJob) {
	key := job.req.ChannelID + "/" + job.req.ThreadTS
	v.mu.Lock()
	defer v.mu.Unlock()
	if q, ok := v.queues[key]; ok {
		q <- job
		return
	}
	q := make(chan voiceJob, 64)
	q <- job
	v.queues[key] = q
	go v.drain(key, q)
}

func (v *voiceSender) drain(key string, q chan voiceJob) {
	for {
		select {
		case job := <-q:
			v.run(context.Background(), job)
		case <-time.After(voiceIdle):
			v.mu.Lock()
			if len(q) == 0 {
				delete(v.queues, key)
				v.mu.Unlock()
				return
			}
			v.mu.Unlock()
		}
	}
}

func (v *voiceSender) run(ctx context.Context, job voiceJob) (out voiceOutcome) {
	began := time.Now()
	out = voiceOutcome{ID: job.id, At: job.received, Lane: job.req.Lane, ChannelID: job.req.ChannelID, ThreadTS: job.req.ThreadTS, Before: job.req.Text, DryRun: job.req.DryRun, QueuedMS: began.Sub(job.received).Milliseconds()}
	defer func() {
		out.TotalMS = time.Since(job.received).Milliseconds()
		v.record(out)
	}()
	id, err := loadIdentity(ctx, v.app.poller.plane.DB())
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if !slices.Contains(id.operatorChannelIDs(), job.req.ChannelID) {
		out.Dropped = "wrong channel: " + job.req.ChannelID + " is not one of the operator's channels"
		return out
	}
	formatted := voiceFormat(job.req.Text)
	out.Formatted, out.FormatMS = formatted, time.Since(began).Milliseconds()
	if reason := voiceMechanical(formatted); reason != "" {
		out.Dropped = reason
		return out
	}
	rules, err := v.rulesAt(job.req.Rules)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	units := voiceUnits(formatted)
	edits := make(chan voiceEdits, 1)
	jevBegan := time.Now()
	key := job.req.ChannelID + "/" + job.req.ThreadTS
	last := v.lastPost(key, job.received)
	go func() { edits <- v.ask(context.WithoutCancel(ctx), rules, job.req.Asked, last.text, units) }()
	var landed *voiceEdits
	select {
	case e := <-edits:
		landed = &e
		out.JevMS, out.Jev = time.Since(jevBegan).Milliseconds(), cmp.Or(e.note, "on time")
	case <-time.After(voiceJevBudget):
		out.JevMS, out.Jev = voiceJevBudget.Milliseconds(), "late: posted as formatted"
	}
	out.After = formatted
	if landed != nil {
		out.After, out.Cut, out.Scores, out.Repeat = voiceApplied(units, *landed), landed.cut, landed.scores, landed.repeat
	}
	if job.req.DryRun {
		if landed == nil {
			e := <-edits
			out.After, out.Cut, out.Scores, out.Repeat, out.Jev = voiceApplied(units, e), e.cut, e.scores, e.repeat, "late: "+cmp.Or(e.note, "answered")
			out.JevMS = time.Since(jevBegan).Milliseconds()
		}
		return out
	}
	postBegan := time.Now()
	postCtx, cancel := context.WithTimeout(ctx, voicePostLimit)
	defer cancel()
	id.Agent = cmp.Or(job.req.Lane, defaultAgent)
	if landed != nil && last.ts != "" && landed.repeat >= voiceRepeatAt {
		_, _, _, err := v.app.newAPI(id.Token).UpdateMessageContext(postCtx, job.req.ChannelID, last.ts, textOption(out.After))
		out.PostMS, out.Edited, out.Permalink = time.Since(postBegan).Milliseconds(), err == nil, id.permalink(job.req.ChannelID, last.ts, job.req.ThreadTS)
		v.posted(key, voiceLast{ts: last.ts, text: out.After, at: job.received})
		if err != nil {
			out.Error = err.Error()
		}
		return out
	}
	res, err := v.app.send(postCtx, v.app.poller.plane.DB(), id, job.req.ChannelID, job.req.ThreadTS, out.After, nil, nil)
	out.PostMS = time.Since(postBegan).Milliseconds()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Permalink = res.Permalink
	v.posted(key, voiceLast{ts: res.TS, text: out.After, at: job.received})
	if landed == nil {
		go v.late(context.WithoutCancel(ctx), id, res, units, out, edits, jevBegan)
	}
	return out
}

func (v *voiceSender) late(ctx context.Context, id identity, res postResult, units []voiceUnit, out voiceOutcome, edits chan voiceEdits, jevBegan time.Time) {
	select {
	case e := <-edits:
		after := voiceApplied(units, e)
		late := voiceOutcome{ID: out.ID + "-late", At: time.Now(), Lane: out.Lane, ChannelID: out.ChannelID, ThreadTS: out.ThreadTS, Before: out.Formatted, Formatted: out.Formatted, After: after, Cut: e.cut, Jev: "late: " + cmp.Or(e.note, "answered"), JevMS: time.Since(jevBegan).Milliseconds(), Permalink: res.Permalink}
		if after != out.Formatted {
			ctx, cancel := context.WithTimeout(ctx, voicePostLimit)
			defer cancel()
			if _, _, _, err := v.app.newAPI(id.Token).UpdateMessageContext(ctx, res.ChannelID, res.TS, textOption(after)); err != nil {
				late.Error = err.Error()
			} else {
				late.Edited = true
			}
		}
		v.record(late)
	case <-time.After(voiceJevLate):
	}
}

func (v *voiceSender) rulesAt(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("voice rules: %w", err)
	}
	v.mu.Lock()
	cached, ok := v.rules[path]
	v.mu.Unlock()
	if ok && cached.mod.Equal(info.ModTime()) {
		return cached.text, nil
	}
	text, err := os.ReadFile(path) //nolint:gosec // G304: the role page the drive names, read to hand Jev
	if err != nil {
		return "", fmt.Errorf("voice rules: %w", err)
	}
	v.mu.Lock()
	v.rules[path] = voiceRules{text: string(text), mod: info.ModTime()}
	v.mu.Unlock()
	return string(text), nil
}

func (v *voiceSender) record(out voiceOutcome) {
	line, err := json.Marshal(out)
	if err != nil {
		v.log.Printf("voice %s: %v", out.ID, err)
		return
	}
	v.write.Lock()
	defer v.write.Unlock()
	f, err := os.OpenFile(v.logTo, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		v.log.Printf("voice %s: %v", out.ID, err)
		return
	}
	_, err = f.Write(append(line, '\n'))
	if err = errors.Join(err, f.Close()); err != nil {
		v.log.Printf("voice %s: %v", out.ID, err)
	}
}

func voiceFormat(text string) string {
	text = voicePRLink.ReplaceAllStringFunc(strings.TrimSpace(text), func(link string) string {
		m := voicePRLink.FindStringSubmatch(link)
		return "<" + m[1] + "|" + prWords(m[2], m[3]) + ">"
	})
	text = barePRLink.ReplaceAllString(text, "$1<$2|#$3>")
	return spaceBeforeStop.ReplaceAllString(slackLinks(text), "$1$2")
}

func prWords(number, words string) string {
	words = strings.TrimSpace(words)
	if own, ok := strings.CutPrefix(words, "#"+number); ok && len(strings.Fields(own)) <= voiceOwnWords {
		return strings.Join(append([]string{"#" + number}, strings.Fields(own)...), " ")
	}
	if fields := strings.Fields(words); len(fields) > 0 && len(fields) <= voiceLabelWord && !strings.HasPrefix(words, "http") {
		return "#" + number + " " + strings.Join(fields, " ")
	}
	return "#" + number
}

func slackLinks(text string) string {
	return bareSlackLink.ReplaceAllStringFunc(text, func(found string) string {
		m := bareSlackLink.FindStringSubmatch(found)
		url := strings.TrimRight(m[2], ".,;:)")
		seconds, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil {
			return found
		}
		label := time.Unix(seconds, 0).In(pacific).Format("3:04 PM") + " message"
		return m[1] + "<" + url + "|" + label + ">" + m[2][len(url):]
	})
}

func voiceMechanical(text string) string {
	if secretShapes.MatchString(text) {
		return "secret: the post holds a token or key"
	}
	if ref := unresolvedRef.FindString(text); ref != "" {
		return "dead link: " + ref + " was never resolved"
	}
	if dead := deadTarget.FindString(text); dead != "" {
		return "dead link: " + dead + " opens nothing he can reach"
	}
	return ""
}

func voiceUnits(text string) []voiceUnit {
	var units []voiceUnit
	lines := strings.Split(text, "\n")
	for n, line := range lines {
		sep := "\n"
		if n == len(lines)-1 {
			sep = ""
		}
		if listLine.MatchString(line) || strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "```") {
			units = append(units, voiceUnit{text: line, after: sep})
			continue
		}
		sentences := sentencesOf(line)
		for i, s := range sentences {
			after := " "
			if i == len(sentences)-1 {
				after = sep
			}
			units = append(units, voiceUnit{text: s, after: after})
		}
	}
	return units
}

func sentencesOf(line string) []string {
	var out []string
	depth, tick, start := 0, false, 0
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '`':
			tick = !tick
		case tick:
		case c == '<':
			depth++
		case c == '>' && depth > 0:
			depth--
		case depth == 0 && (c == '.' || c == '?' || c == '!') && i+2 < len(line) && line[i+1] == ' ' && startsSentence(line[i+2]):
			out = append(out, strings.TrimSpace(line[start:i+1]))
			start = i + 2
		}
	}
	return append(out, strings.TrimSpace(line[start:]))
}

func startsSentence(c byte) bool {
	return (c >= 'A' && c <= 'Z') || c == '<' || c == '*' || c == '_' || c == '"' || c == '#'
}

func spokenUnits(units []voiceUnit) ([]string, map[string]int) {
	ids, at := []string{}, map[string]int{}
	for n, u := range units {
		if strings.TrimSpace(u.text) == "" || strings.HasPrefix(strings.TrimSpace(u.text), "```") || headingLine.MatchString(u.text) || len(ids) == voiceMaxUnits {
			continue
		}
		id := "u" + strconv.Itoa(len(ids)+1)
		ids, at[id] = append(ids, id), n
	}
	return ids, at
}

type voiceState struct {
	Rules    string            `json:"rules"`
	Asked    string            `json:"asked,omitempty"`
	Previous string            `json:"previous,omitempty"`
	Post     map[string]string `json:"post"`
	Order    []string          `json:"order"`
}

func (v *voiceSender) ask(ctx context.Context, rules, asked, previous string, units []voiceUnit) voiceEdits {
	ids, at := spokenUnits(units)
	if len(ids) < 2 && previous == "" {
		return voiceEdits{note: "one unit: nothing to cut"}
	}
	post := make(map[string]string, len(ids))
	questions := map[string]spawnllm.Question{}
	if previous != "" {
		questions["repeat"] = spawnllm.Binary{
			Instructions: "Does `post` tell him nothing `previous`, the lane's last post minutes ago, did not already tell him: the same news, decision or ask in other words?",
			Yes:          "A repeat: every fact and ask in it was already in the previous post",
			No:           "New: it carries at least one fact, change, decision or ask the previous post did not",
		}
	}
	for _, id := range ids {
		post[id] = units[at[id]].text
		questions["cut_"+id] = spawnllm.Binary{
			Instructions: "Following `rules` and its bad and good examples, is unit " + id + " of `post` something he does not need: how the agents, sessions, builders, readers, card queues, the old drive or its tools work inside, a promise to fix tooling, his own words restated, an apology, a PR title or list repeating what another unit already names, or progress that changes nothing for him? It is never something a later unit refers back to, such as the step an \"If it does\" depends on.",
			Yes:          "Cut it: the post reads the same to him without it",
			No:           "Keep it: it carries the answer, a change for him, when he hears next, what needs him, or a fact the other units lean on",
		}
	}
	decision, err := v.decide(ctx, voiceState{Rules: rules, Asked: asked, Previous: previous, Post: post, Order: ids}, questions, spawnllm.DecideOpts{Timeout: voiceJevLate})
	if err != nil {
		return voiceEdits{note: "failed: " + err.Error()}
	}
	e := voiceEdits{scores: map[string]float64{}}
	if repeat, ok := decision.Answers["repeat"].(spawnllm.BinaryAnswer); ok {
		e.repeat = repeat.PYes
	}
	for n, id := range ids {
		answer, ok := decision.Answers["cut_"+id].(spawnllm.BinaryAnswer)
		e.scores[id] = answer.PYes
		if ok && answer.PYes >= voiceCutAt && (n > 0 || answer.PYes >= voiceOpenerCutAt) {
			e.cut = append(e.cut, id)
		}
	}
	return e
}

func voiceApplied(units []voiceUnit, e voiceEdits) string {
	_, at := spokenUnits(units)
	cut := map[int]bool{}
	for _, id := range e.cut {
		if n, ok := at[id]; ok {
			cut[n] = true
		}
	}
	var b strings.Builder
	for n, u := range units {
		if cut[n] {
			if strings.Contains(u.after, "\n") {
				b.WriteString(u.after)
			}
			continue
		}
		b.WriteString(u.text)
		b.WriteString(u.after)
	}
	return tidyVoice(b.String())
}

var (
	spaceBeforeBreak = regexp.MustCompile(`[ \t]+\n`)
	extraBreaks      = regexp.MustCompile(`\n{3,}`)
)

func tidyVoice(text string) string {
	lines := strings.Split(spaceBeforeBreak.ReplaceAllString(text, "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for n, line := range lines {
		if headingLine.MatchString(line) && !sectionFollows(lines[n+1:]) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(extraBreaks.ReplaceAllString(strings.Join(kept, "\n"), "\n\n"))
}

func sectionFollows(rest []string) bool {
	for _, line := range rest {
		if strings.TrimSpace(line) != "" {
			return !headingLine.MatchString(line)
		}
	}
	return false
}

func readVoiceLog(path string, last int) ([]voiceOutcome, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the daemon's own voice log
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var all []voiceOutcome
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scan.Scan() {
		var o voiceOutcome
		if err := json.Unmarshal(scan.Bytes(), &o); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, o)
	}
	if err := scan.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return all[max(0, len(all)-last):], nil
}

type voiceLast struct {
	ts   string
	text string
	at   time.Time
}

func (v *voiceSender) lastPost(key string, now time.Time) voiceLast {
	v.mu.Lock()
	defer v.mu.Unlock()
	if last, ok := v.last[key]; ok && now.Sub(last.at) <= voiceRepeatFor {
		return last
	}
	return voiceLast{}
}

func (v *voiceSender) posted(key string, last voiceLast) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.last[key] = last
}
