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
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/procs"
	spawnllm "github.com/yasyf/spawnllm/go"
)

const (
	voicePath         = "/voice"
	voicePostsPath    = voicePath + "/posts"
	voiceSettlePath   = voicePath + "/settle"
	voiceReactPath    = voicePath + "/react"
	voiceLinkPath     = voicePath + "/link"
	voiceStateBlock   = "voice-state"
	voiceJevBudget    = 600 * time.Millisecond
	voiceJevLate      = 5 * time.Second
	voicePostLimit    = 10 * time.Second
	voiceCutAt        = 0.65
	voiceOpenerCutAt  = 0.9
	voiceRepeatAt     = 0.8
	voiceRepeatFor    = 10 * time.Minute
	voiceMaxUnits     = 16
	voiceLabelWord    = 10
	voiceOwnWords     = 10
	voiceBodyBytes    = 64 << 10
	voiceIdle         = time.Minute
	voiceLogName      = "voice.jsonl"
	voiceMaxOptions   = 5
	voiceOptionRunes  = 24
	voiceOptionWords  = 4
	voiceSectionRunes = 3000
	voiceOptionAction = cardActionPrefix + ":voice:"
	voiceHoldAt       = 0.7
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
	Lane      string              `json:"lane"`
	Drive     string              `json:"drive,omitempty"`
	ChannelID string              `json:"channel_id"`
	ThreadTS  string              `json:"thread_ts,omitempty"`
	Text      string              `json:"text"`
	Rules     string              `json:"rules"`
	Asked     string              `json:"asked,omitempty"`
	Options   []string            `json:"options,omitempty"`
	PRs       []voicePR           `json:"prs,omitempty"`
	EditTS    string              `json:"edit_ts,omitempty"`
	About     []map[string]string `json:"about,omitempty"`
	DryRun    bool                `json:"dry_run,omitempty"`
}

type voiceOutcome struct {
	ID        string              `json:"id"`
	At        time.Time           `json:"at"`
	Lane      string              `json:"lane"`
	ChannelID string              `json:"channel_id"`
	ThreadTS  string              `json:"thread_ts,omitempty"`
	Before    string              `json:"before"`
	Formatted string              `json:"formatted"`
	After     string              `json:"after"`
	Repeat    float64             `json:"repeat,omitempty"`
	Scores    map[string]float64  `json:"scores,omitempty"`
	Cut       []string            `json:"cut,omitempty"`
	Dropped   string              `json:"dropped,omitempty"`
	Jev       string              `json:"jev"`
	Edited    bool                `json:"edited,omitempty"`
	Permalink string              `json:"permalink,omitempty"`
	Error     string              `json:"error,omitempty"`
	DryRun    bool                `json:"dry_run,omitempty"`
	TS        string              `json:"ts,omitempty"`
	PRs       []voicePR           `json:"prs,omitempty"`
	About     []map[string]string `json:"about,omitempty"`
	Held      string              `json:"held,omitempty"`
	Options   []string            `json:"options,omitempty"`
	QueuedMS  int64               `json:"queued_ms"`
	FormatMS  int64               `json:"format_ms"`
	JevMS     int64               `json:"jev_ms"`
	PostMS    int64               `json:"post_ms"`
	TotalMS   int64               `json:"total_ms"`
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
	if _, err := callerWindow(r); err != nil && !laneSupervisor(r) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == voicePostsPath && r.Method == http.MethodGet:
		v.servePosts(w, r)
		return
	case r.URL.Path == voiceSettlePath && r.Method == http.MethodPost:
		v.serveSettle(w, r)
		return
	case r.URL.Path == voiceReactPath && r.Method == http.MethodPost:
		v.serveReact(w, r)
		return
	case r.URL.Path == voiceLinkPath && r.Method == http.MethodPost:
		v.serveLink(w, r)
		return
	case r.URL.Path != voicePath || r.Method != http.MethodPost:
		http.NotFound(w, r)
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
	if err := voiceOptionsCheck(req.Options); err != nil {
		v.refused(req, err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	why, attached := v.held(r.Context(), req)
	if why != "" && !req.DryRun {
		v.refused(req, why)
		http.Error(w, why, http.StatusUnprocessableEntity)
		return
	}
	if len(req.Options) == 0 {
		req.Options = attached
	}
	job := voiceJob{req: req, id: voiceID(), received: time.Now()}
	w.Header().Set("Content-Type", "application/json")
	if req.DryRun {
		out := v.run(r.Context(), job)
		out.Held, out.Options = why, req.Options
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	v.enqueue(job)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": job.id, "attached": attached})
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
	id.Agent, id.Drive = "claude", job.req.Drive
	out.PRs, out.About, out.Options = job.req.PRs, job.req.About, job.req.Options
	if job.req.EditTS != "" {
		return v.editPost(postCtx, id, job, out, units, landed, edits, jevBegan, postBegan, key)
	}
	if len(job.req.Options) > 0 {
		return v.postQuestion(postCtx, id, job, out, units, landed, edits, jevBegan, postBegan, key)
	}
	if landed != nil && last.ts != "" && landed.repeat >= voiceRepeatAt {
		_, _, _, err := v.app.newAPI(id.Token).UpdateMessageContext(postCtx, job.req.ChannelID, last.ts, textOption(out.After))
		out.PostMS, out.Edited, out.Permalink = time.Since(postBegan).Milliseconds(), err == nil, id.permalink(job.req.ChannelID, last.ts, job.req.ThreadTS)
		out.TS = last.ts
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
	out.Permalink, out.TS = res.Permalink, res.TS
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
			Instructions: "Is `post` a second take on the news `previous`, the lane's post minutes ago, already gave him: the same PRs and the same state, retold or with its ask added, so that he needs only one of the two?",
			Yes:          "A second take: one post would have said both",
			No:           "Different news: another PR, another state, or a change since the previous post",
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
	e.cut = leaned(ids, post, e.cut)
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

var leansBack = regexp.MustCompile(`^(?:[*_]*)(?:It|Its|It's|This|That|These|Those|They|Their|Them|Then|So|If (?:it|so|that|they)|When (?:it|that|they)|Once (?:it|that|they)|Both|Neither|Either|The (?:same|other|second|first|fix|one)|Which)\b`)

func leaned(ids []string, post map[string]string, cut []string) []string {
	cutting := map[string]bool{}
	for _, id := range cut {
		cutting[id] = true
	}
	for n := len(ids) - 1; n > 0; n-- {
		if !cutting[ids[n]] && cutting[ids[n-1]] && leansBack.MatchString(strings.TrimSpace(post[ids[n]])) {
			delete(cutting, ids[n-1])
		}
	}
	kept := cut[:0:0]
	for _, id := range cut {
		if cutting[id] {
			kept = append(kept, id)
		}
	}
	return kept
}

func voiceOptionsCheck(options []string) error {
	if len(options) > voiceMaxOptions {
		return fmt.Errorf("voice takes at most %d options, not %d", voiceMaxOptions, len(options))
	}
	seen := map[string]bool{}
	for _, option := range options {
		words := len(strings.Fields(option))
		if words == 0 || words > voiceOptionWords || runes(option) > voiceOptionRunes {
			return fmt.Errorf("refused, not posted: button %q is too long. Slack cuts a button at about 30 characters (his words: \"the buttons are all cut off I have no idea what they're saying\"), so a button is 1 to %d words of at most %d characters, such as \"Approve\", \"Approve + sidebar\", \"Make optional\" or \"Hold\", and the post lists each option in full under the same short name", option, voiceOptionWords, voiceOptionRunes)
		}
		if key := strings.ToLower(strings.TrimSpace(option)); seen[key] {
			return fmt.Errorf("refused, not posted: two buttons read %q; each button must read differently at a glance", option)
		} else {
			seen[key] = true
		}
	}
	return nil
}

// postQuestion posts a lane's question with a button per option, waiting on Jev's edits rather than editing late, since a late text edit would drop the buttons, and never cutting its first line, the question a tap names and a card's tap is matched by.
func (v *voiceSender) postQuestion(ctx context.Context, id identity, job voiceJob, out voiceOutcome, units []voiceUnit, landed *voiceEdits, edits chan voiceEdits, jevBegan, postBegan time.Time, key string) voiceOutcome {
	if landed == nil {
		e := <-edits
		landed = &e
		out.Scores, out.Repeat, out.Jev = e.scores, e.repeat, "late: "+cmp.Or(e.note, "answered")
		out.JevMS = time.Since(jevBegan).Milliseconds()
	}
	kept := *landed
	kept.cut = slices.DeleteFunc(slices.Clone(kept.cut), func(id string) bool { ids, _ := spokenUnits(units); return len(ids) > 0 && id == ids[0] })
	out.After, out.Cut = voiceApplied(units, kept), kept.cut
	res, err := v.app.send(ctx, v.app.poller.plane.DB(), id, job.req.ChannelID, job.req.ThreadTS, out.After, voiceQuestionBlocks(out.After, job.req.Options), nil)
	out.PostMS = time.Since(postBegan).Milliseconds()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Permalink, out.TS = res.Permalink, res.TS
	v.posted(key, voiceLast{ts: res.TS, text: out.After, at: job.received})
	go v.linkEarlier(context.WithoutCancel(ctx), job, res.TS, out.After)
	return out
}

func voiceQuestionBlocks(text string, options []string) []slack.Block {
	blocks := voiceSections(text)
	buttons := make([]slack.BlockElement, 0, len(options))
	for n, option := range options {
		button := slack.NewButtonBlockElement(fmt.Sprintf("%s%d", voiceOptionAction, n), option, slack.NewTextBlockObject(slack.PlainTextType, option, false, false))
		if n == 0 {
			button.Style = slack.StylePrimary
		}
		buttons = append(buttons, button)
	}
	return append(blocks, slack.NewActionBlock("voice-options", buttons...))
}

func voiceSections(text string) []slack.Block {
	var blocks []slack.Block
	var chunk strings.Builder
	flush := func() {
		if chunk.Len() > 0 {
			blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, chunk.String(), false, false), nil, nil))
			chunk.Reset()
		}
	}
	for _, paragraph := range strings.SplitAfter(explicitLinks(text), "\n\n") {
		if runes(chunk.String())+runes(paragraph) > voiceSectionRunes {
			flush()
		}
		chunk.WriteString(paragraph)
	}
	flush()
	return blocks
}

func voiceAnswered(blocks []slack.Block, label string, at time.Time) []slack.Block {
	kept := slices.DeleteFunc(slices.Clone(blocks), func(b slack.Block) bool { return b.BlockType() == slack.MBTAction })
	return append(kept, slack.NewContextBlock("voice-answered", slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("✅ *%s* · %s", mrkdwnEscapes.Replace(label), at.In(pacific).Format("3:04 PM")), false, false)))
}

const voiceTypedChoice = "refused, not posted: this asks him to type a word to choose, and a question to him is always buttons. Post it again with each choice as an option (lane say \"<question>\" --options \"<recommended>\" --options \"<other>\")"

const voiceMiscount = "refused, not posted: it counts a set (such as \"all six\") but lists a different number of them. List every member of a set you count, or fix the number, then post it again"

type voicePR struct {
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	State    string `json:"state,omitempty"`
	Head     string `json:"head,omitempty"`
	Approved bool   `json:"approved,omitempty"`
}

const voiceMaxApprovePRs = 2

// held is Jev's one read before a post queues of what his rules refuse outright: a post with no options asking him to type his pick, which a button would carry; a post whose count of a set disagrees with the members it lists, by his rule to list every member of a set you count; and a post asking his Approve or tap on a PR, which never goes out without the buttons it asks him to press and never about a PR already merged or closed. It answers why the post is held, or "", and the Approve and Hold buttons to attach to an ask for his Approve that carries none; a Jev that cannot answer within the budget lets the post through as written.
func (v *voiceSender) held(ctx context.Context, req voiceRequest) (string, []string) {
	questions := map[string]spawnllm.Question{"miscount": spawnllm.Binary{
		Instructions: "Does `post` give a number for a set of things (\"all six\", \"three PRs\", \"both\") and then list members of that set, where the number of members it lists differs from the number it gives?",
		Yes:          "The count and the listed members disagree",
		No:           "It counts nothing it lists, or the count matches the members it lists",
	}}
	if len(req.Options) == 0 {
		questions["typed"] = spawnllm.Binary{
			Instructions: "Does `post` ask him to type or reply a word or phrase to choose between options, such as \"say hotfix or wait\", \"reply merge\" or \"tell me A or B\", where a button per option would carry the choice?",
			Yes:          "It asks him to type his pick",
			No:           "It asks him nothing to pick, or asks an open question no fixed set of buttons could answer",
		}
	}
	if len(req.PRs) > 0 {
		questions["approve"] = spawnllm.Binary{
			Instructions: "Does `post` ask him to approve, tap, merge or otherwise act on one of the PRs it links, such as \"needs your Approve\", \"on your tap\" or \"merges once you approve\"?",
			Yes:          "It asks his Approve or tap on a PR",
			No:           "It only tells him about the PRs and asks nothing of him on them",
		}
	}
	ctx, cancel := context.WithTimeout(ctx, voiceJevBudget)
	defer cancel()
	decision, err := v.decide(ctx, voiceState{Post: map[string]string{"post": req.Text}, Order: []string{"post"}}, questions, spawnllm.DecideOpts{Timeout: voiceJevBudget})
	if err != nil {
		return "", nil
	}
	yes := func(key string) bool {
		answer, ok := decision.Answers[key].(spawnllm.BinaryAnswer)
		return ok && answer.PYes >= voiceHoldAt
	}
	var attached []string
	if yes("approve") {
		var why string
		if why, attached = voiceApproveAsk(req); why != "" {
			return why, nil
		}
	}
	switch {
	case yes("typed") && len(attached) == 0:
		return voiceTypedChoice, nil
	case yes("miscount"):
		return voiceMiscount, nil
	}
	return "", attached
}

func voiceApproveAsk(req voiceRequest) (string, []string) {
	var settled []string
	for _, pr := range req.PRs {
		if pr.State != "" && pr.State != "OPEN" {
			settled = append(settled, fmt.Sprintf("#%d is %s", pr.Number, strings.ToLower(pr.State)))
		}
	}
	if len(settled) > 0 {
		return "refused, not posted: it asks his Approve or tap on a PR that is no longer open (" + strings.Join(settled, ", ") + "). Say what happened to it instead", nil
	}
	if !slices.ContainsFunc(req.PRs, func(pr voicePR) bool { return !pr.Approved }) {
		return fmt.Sprintf("refused, not posted: he already approved %s at its head, so it is decided (\"if I approve something its approved\"). Land it, and ask him nothing about it", voicePRNames(req.PRs)), nil
	}
	if len(req.Options) > 0 {
		return "", nil
	}
	if len(req.PRs) > voiceMaxApprovePRs {
		return fmt.Sprintf("refused, not posted: it asks his Approve on %d PRs with no buttons. Post it with --options naming what each button does", len(req.PRs)), nil
	}
	var options []string
	for _, pr := range req.PRs {
		options = append(options, fmt.Sprintf("Approve #%d", pr.Number), fmt.Sprintf("Hold #%d", pr.Number))
	}
	return "", options
}

// refused logs a post /voice turned away before it queued, with its lane and why, so a lane refused again and again shows in voice.jsonl.
func (v *voiceSender) refused(req voiceRequest, why string) {
	v.record(voiceOutcome{ID: voiceID(), At: time.Now(), Lane: req.Lane, ChannelID: req.ChannelID, ThreadTS: req.ThreadTS, Before: req.Text, Formatted: req.Text, Dropped: "refused: " + why})
}

// editPost rewrites a lane's earlier post in place, its buttons redrawn when the edit carries options, so a post whose subject changed is corrected where he read it rather than retold.
func (v *voiceSender) editPost(ctx context.Context, id identity, job voiceJob, out voiceOutcome, units []voiceUnit, landed *voiceEdits, edits chan voiceEdits, jevBegan, postBegan time.Time, key string) voiceOutcome {
	if landed == nil {
		e := <-edits
		landed = &e
		out.JevMS, out.Jev = time.Since(jevBegan).Milliseconds(), "late: "+cmp.Or(e.note, "answered")
	}
	out.After, out.Cut, out.Scores = voiceApplied(units, *landed), landed.cut, landed.scores
	blocks := voiceSections(out.After)
	if len(job.req.Options) > 0 {
		blocks = voiceQuestionBlocks(out.After, job.req.Options)
	}
	err := updateMessage(ctx, v.app.newAPI(id.Token), job.req.ChannelID, job.req.EditTS, out.After, blocks)
	out.PostMS, out.TS, out.Edited, out.Permalink = time.Since(postBegan).Milliseconds(), job.req.EditTS, err == nil, id.permalink(job.req.ChannelID, job.req.EditTS, job.req.ThreadTS)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	v.posted(key, voiceLast{ts: job.req.EditTS, text: out.After, at: job.received})
	return out
}

// servePosts answers the lane posts the voice sent since a moment, each with the subjects it named, and his taps on its questions by question ts, so the drive can change a post when what it says changes underneath it.
func (v *voiceSender) servePosts(w http.ResponseWriter, r *http.Request) {
	since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil {
		http.Error(w, "voice posts needs since as RFC 3339", http.StatusBadRequest)
		return
	}
	f, err := os.Open(v.logTo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	posts, taps, asks := []voiceOutcome{}, map[string]string{}, map[string][]map[string]string{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for scan.Scan() {
		var out voiceOutcome
		if json.Unmarshal(scan.Bytes(), &out) != nil {
			continue
		}
		if strings.HasSuffix(out.ID, "-tapped") && !out.At.Before(since) {
			taps[out.TS] = out.After
			continue
		}
		if strings.HasSuffix(out.ID, "-asks") {
			asks[out.TS] = append(asks[out.TS], out.About...)
			continue
		}
		if out.DryRun || out.Permalink == "" || out.At.Before(since) || strings.HasSuffix(out.ID, "-late") {
			continue
		}
		out.TS = cmp.Or(out.TS, permalinkTS(out.Permalink))
		posts = append(posts, out)
	}
	for n := range posts {
		posts[n].About = append(posts[n].About, asks[posts[n].TS]...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"posts": posts, "taps": taps})
}

func permalinkTS(link string) string {
	ref, err := parsePermalink(link)
	if err != nil {
		return ""
	}
	return ref.ts
}

type voiceSettle struct {
	ChannelID     string `json:"channel_id"`
	TS            string `json:"ts"`
	ThreadTS      string `json:"thread_ts,omitempty"`
	Line          string `json:"line"`
	Buttons       bool   `json:"keep_buttons,omitempty"`
	OnlyIfButtons bool   `json:"only_if_buttons,omitempty"`
}

// serveSettle marks a lane post with the state of what it named, such as "✅ #57894 merged 3:18 PM", in one line under it that replaces the last such line, its buttons dropped unless keep_buttons: the mechanical half of a post following its subjects.
func (v *voiceSender) serveSettle(w http.ResponseWriter, r *http.Request) {
	var s voiceSettle
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&s); err != nil || s.ChannelID == "" || !slackTSPattern.MatchString(s.TS) || strings.TrimSpace(s.Line) == "" {
		http.Error(w, "voice settle needs channel_id, ts and line", http.StatusBadRequest)
		return
	}
	id, err := loadIdentity(r.Context(), v.app.poller.plane.DB())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !slices.Contains(id.operatorChannelIDs(), s.ChannelID) {
		http.Error(w, s.ChannelID+" is not one of the operator's channels", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), voicePostLimit)
	defer cancel()
	api := v.app.newAPI(id.Token)
	m, err := messageAt(ctx, api, s.ChannelID, cmp.Or(s.ThreadTS, s.TS), s.TS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if s.OnlyIfButtons && !slices.ContainsFunc(m.Blocks.BlockSet, func(b slack.Block) bool { return b.BlockType() == slack.MBTAction }) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"permalink": ""})
		return
	}
	blocks := voiceSettled(m.Blocks.BlockSet, m.Text, s.Line, s.Buttons)
	if err := updateMessage(ctx, api, s.ChannelID, s.TS, m.Text, blocks); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	v.record(voiceOutcome{ID: voiceID() + "-settled", At: time.Now(), ChannelID: s.ChannelID, ThreadTS: s.ThreadTS, TS: s.TS, Before: m.Text, Formatted: m.Text, After: s.Line, Edited: true, DryRun: true})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"permalink": id.permalink(s.ChannelID, s.TS, s.ThreadTS)})
}

func voiceSettled(blocks []slack.Block, text, line string, buttons bool) []slack.Block {
	if !slices.ContainsFunc(blocks, func(b slack.Block) bool { return b.BlockType() != slack.MBTRichText }) {
		blocks = voiceSections(text)
	}
	kept := slices.DeleteFunc(slices.Clone(blocks), func(b slack.Block) bool {
		if c, ok := b.(*slack.ContextBlock); ok && c.BlockID == voiceStateBlock {
			return true
		}
		return !buttons && b.BlockType() == slack.MBTAction
	})
	return append(kept, slack.NewContextBlock(voiceStateBlock, slack.NewTextBlockObject(slack.MarkdownType, line, false, false)))
}

func voicePRNames(prs []voicePR) string {
	names := make([]string, 0, len(prs))
	for _, pr := range prs {
		names = append(names, fmt.Sprintf("#%d", pr.Number))
	}
	return strings.Join(names, ", ")
}

// voiceTapped logs the operator's tap on a voice question, by the question's ts and the label he pressed, as one appended line beside the sender's own, so a post naming that card follows his answer.
func voiceTapped(channelID, ts, label string) error {
	line, err := json.Marshal(voiceOutcome{ID: voiceID() + "-tapped", At: time.Now(), ChannelID: channelID, TS: ts, After: label, DryRun: true})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(appPaths().StateDir(), voiceLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

// laneSupervisor reads whether a request comes from a drive's lane supervisor, a launchd process and no Claude session, by its pid header naming that process or one under it: its argv runs lane with supervise.
func laneSupervisor(r *http.Request) bool {
	pid, err := strconv.Atoi(r.Header.Get(toolsPIDHeader))
	if err != nil {
		return false
	}
	return procs.FindAncestor(pid, func(argv []string) bool {
		return slices.Contains(argv, "supervise") && slices.ContainsFunc(argv, func(arg string) bool { return arg == "lane" || strings.HasSuffix(arg, "/lane") })
	}) != 0
}

type voiceReact struct {
	ChannelID string   `json:"channel_id"`
	TS        string   `json:"ts"`
	Add       string   `json:"add,omitempty"`
	Remove    []string `json:"remove,omitempty"`
}

// serveReact swaps reactions on his message in one of his channels, such as 🚧 for 👀 when a lane's owner starts on it, so the reaction says where his message stands.
func (v *voiceSender) serveReact(w http.ResponseWriter, r *http.Request) {
	var s voiceReact
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&s); err != nil || s.ChannelID == "" || !slackTSPattern.MatchString(s.TS) {
		http.Error(w, "voice react needs channel_id and ts", http.StatusBadRequest)
		return
	}
	id, err := loadIdentity(r.Context(), v.app.poller.plane.DB())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !slices.Contains(id.operatorChannelIDs(), s.ChannelID) {
		http.Error(w, s.ChannelID+" is not one of the operator's channels", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), voicePostLimit)
	defer cancel()
	api, ref := v.app.newAPI(id.Token), slack.NewRefToMessage(s.ChannelID, s.TS)
	for _, name := range s.Remove {
		if err := api.RemoveReactionContext(ctx, name, ref); err != nil && errorCode(err) != "no_reaction" {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	if s.Add != "" {
		if err := api.AddReactionContext(ctx, s.Add, ref); err != nil && errorCode(err) != "already_reacted" {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type voiceLink struct {
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	CardTS    string `json:"card_ts"`
}

// serveLink records that an earlier post asks the question a card asks, so the post settles with the card's answer.
func (v *voiceSender) serveLink(w http.ResponseWriter, r *http.Request) {
	var s voiceLink
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&s); err != nil || !slackTSPattern.MatchString(s.TS) || !slackTSPattern.MatchString(s.CardTS) {
		http.Error(w, "voice link needs ts and card_ts", http.StatusBadRequest)
		return
	}
	v.linked(s.ChannelID, s.TS, s.CardTS)
	w.WriteHeader(http.StatusNoContent)
}

func (v *voiceSender) linked(channelID, ts, cardTS string) {
	v.record(voiceOutcome{ID: voiceID() + "-asks", At: time.Now(), ChannelID: channelID, TS: ts, About: []map[string]string{{"kind": "card", "ts": cardTS}}, DryRun: true})
}

// linkEarlier asks Jev, once per earlier lane post in the card's thread within FOLLOWED hours, whether it asks the question the card asks, and links each that does, so a typed ask the card replaced settles when he taps the card.
func (v *voiceSender) linkEarlier(ctx context.Context, job voiceJob, cardTS, question string) {
	earlier := v.threadPosts(job.req.ChannelID, job.req.ThreadTS, job.req.Lane, cardTS)
	if len(earlier) == 0 {
		return
	}
	post := map[string]string{"card": question}
	questions := map[string]spawnllm.Question{}
	for n, e := range earlier {
		key := fmt.Sprintf("p%d", n)
		post[key] = e.After
		questions[key] = spawnllm.Binary{
			Instructions: "Does `post` " + key + " ask him the same question `post` card asks, so his answer on the card answers it too?",
			Yes:          "The same question",
			No:           "Another question, or no question",
		}
	}
	decision, err := v.decide(ctx, voiceState{Post: post, Order: slices.Sorted(maps.Keys(post))}, questions, spawnllm.DecideOpts{Timeout: voiceJevLate})
	if err != nil {
		v.log.Printf("voice link %s: %v", cardTS, err)
		return
	}
	for n, e := range earlier {
		if answer, ok := decision.Answers[fmt.Sprintf("p%d", n)].(spawnllm.BinaryAnswer); ok && answer.PYes >= voiceHoldAt {
			v.linked(job.req.ChannelID, e.TS, cardTS)
		}
	}
}

// threadPosts are the lane's posts in one thread of a channel in the last day, before the given ts.
func (v *voiceSender) threadPosts(channelID, threadTS, lane, before string) []voiceOutcome {
	f, err := os.Open(v.logTo)
	if err != nil {
		return nil
	}
	defer f.Close()
	var found []voiceOutcome
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 1<<20), 8<<20)
	since := time.Now().Add(-24 * time.Hour)
	for scan.Scan() {
		var out voiceOutcome
		if json.Unmarshal(scan.Bytes(), &out) != nil || out.DryRun || out.Lane != lane || out.ChannelID != channelID || out.At.Before(since) {
			continue
		}
		ts := cmp.Or(out.TS, permalinkTS(out.Permalink))
		if ts == "" || ts >= before || cmp.Or(out.ThreadTS, ts) != cmp.Or(threadTS, before) {
			continue
		}
		out.TS = ts
		found = append(found, out)
	}
	return found
}
