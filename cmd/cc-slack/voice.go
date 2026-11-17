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
	voicePath          = "/voice"
	voicePostsPath     = voicePath + "/posts"
	voiceSettlePath    = voicePath + "/settle"
	voiceReactPath     = voicePath + "/react"
	voiceLinkPath      = voicePath + "/link"
	voiceStatusPath    = voicePath + "/status"
	voiceRuledPath     = voicePath + "/ruled"
	voiceWarnFor       = 30 * time.Minute
	voiceOpenCards     = 3
	voiceBackfillEvery = 10 * time.Minute
	voiceStateBlock    = "voice-state"
	voiceJevBudget     = 600 * time.Millisecond
	voiceJevLate       = 5 * time.Second
	voicePostLimit     = 10 * time.Second
	voiceCutAt         = 0.65
	voiceOpenerCutAt   = 0.9
	voiceEditFor       = 24 * time.Hour
	voiceRepeatAt      = 0.8
	voiceRepeatFor     = time.Hour
	voiceMaxUnits      = 16
	voiceBodyBytes     = 64 << 10
	voiceIdle          = time.Minute
	voiceLogName       = "voice.jsonl"
	voiceMaxOptions    = 5
	voiceSectionRunes  = 3000
	voiceOptionAction  = cardActionPrefix + ":voice:"
	voiceHoldAt        = 0.7
)

var (
	voicePRLink     = regexp.MustCompile(`<(https://github\.com/[\w.-]+/[\w.-]+/pull/(\d+))(?:\|([^<>]*))?>`)
	barePRLink      = regexp.MustCompile(`(^|[\s(])(https://github\.com/[\w.-]+/[\w.-]+/pull/(\d+))\b`)
	bareSlackLink   = regexp.MustCompile(`(^|[\s(])(https://[\w-]+\.slack\.com/archives/[A-Z0-9]+/p(\d{10})\d{6}[^\s<>]*)`)
	doubledLink     = regexp.MustCompile(`<(<[^<>\s][^<>]*>)>`)
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
	Files     []string            `json:"files,omitempty"`
	Words     string              `json:"words,omitempty"`
	Card      string              `json:"card,omitempty"`
	Rulings   []voiceRuling       `json:"rulings,omitempty"`
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
	Words     string              `json:"words,omitempty"`
	Sent      bool                `json:"sent"`
	Score     *float64            `json:"score,omitempty"`
	Verdict   string              `json:"verdict,omitempty"`
	Reason    string              `json:"reason,omitempty"`
	Warning   string              `json:"warning,omitempty"`
	PRs       []voicePR           `json:"prs,omitempty"`
	About     []map[string]string `json:"about,omitempty"`
	Held      string              `json:"held,omitempty"`
	Options   []string            `json:"options,omitempty"`
	Question  bool                `json:"question,omitempty"`
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
	score    float64
}

type voiceDecide func(ctx context.Context, state any, questions map[string]spawnllm.Question, opts spawnllm.DecideOpts) (spawnllm.Decision, error)

type voiceSender struct {
	app    *app
	decide voiceDecide
	logTo  string
	log    *log.Logger

	mu        sync.Mutex
	queues    map[string]chan voiceJob
	last      map[string]voiceLast
	rules     map[string]voiceRules
	write     sync.Mutex
	warned    map[string]time.Time
	questions map[string]bool
	looked    map[string]bool
}

type voiceRules struct {
	text string
	mod  time.Time
}

func newVoiceSender(a *app, decide voiceDecide) *voiceSender {
	return &voiceSender{
		app:       a,
		decide:    decide,
		logTo:     filepath.Join(appPaths().StateDir(), voiceLogName),
		log:       log.New(os.Stderr, "["+binaryName+" voice] ", log.LstdFlags),
		queues:    map[string]chan voiceJob{},
		last:      map[string]voiceLast{},
		rules:     map[string]voiceRules{},
		warned:    map[string]time.Time{},
		questions: map[string]bool{},
		looked:    map[string]bool{},
	}
}

func (v *voiceSender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := callerWindow(r); err != nil && !laneSupervisor(r) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	serve, ok := map[string]http.HandlerFunc{
		http.MethodPost + " " + voicePath:       v.servePost,
		http.MethodGet + " " + voicePostsPath:   v.servePosts,
		http.MethodPost + " " + voiceSettlePath: v.serveSettle,
		http.MethodPost + " " + voiceReactPath:  v.serveReact,
		http.MethodPost + " " + voiceLinkPath:   v.serveLink,
		http.MethodPost + " " + voiceStatusPath: v.serveStatus,
		http.MethodPost + " " + voiceRuledPath:  v.serveRuled,
	}[r.Method+" "+r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	serve(w, r)
}

func (v *voiceSender) servePost(w http.ResponseWriter, r *http.Request) {
	var req voiceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || strings.TrimSpace(req.Text) == "" || req.Rules == "" {
		http.Error(w, "voice needs channel_id, text and rules", http.StatusBadRequest)
		return
	}
	if err := v.editAt(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := voiceOptionsCheck(req.Options); err != nil && req.DryRun {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(voiceOutcome{Lane: req.Lane, ChannelID: req.ChannelID, Before: req.Text, DryRun: true, Verdict: "refused", Reason: err.Error()})
		return
	} else if err != nil {
		v.refused(req, err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	why, attached, warning, score := v.held(r.Context(), req)
	why = v.retried(&req, why)
	if why != "" && !req.DryRun {
		v.refused(req, why)
		http.Error(w, why, http.StatusUnprocessableEntity)
		return
	}
	if len(req.Options) == 0 {
		req.Options = attached
	}
	job := voiceJob{req: req, id: voiceID(), received: time.Now(), score: score}
	w.Header().Set("Content-Type", "application/json")
	if req.DryRun {
		out := v.run(r.Context(), job)
		out.Held, out.Options, out.Warning = cmp.Or(why, warning), req.Options, warning
		out.Verdict, out.Reason = "ok", cmp.Or(why, out.Dropped, out.Error)
		if out.Reason != "" {
			out.Verdict = "refused"
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	v.enqueue(job)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": job.id, "attached": attached, "warning": warning})
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
	out = voiceOutcome{ID: job.id, At: job.received, Lane: job.req.Lane, ChannelID: job.req.ChannelID, ThreadTS: job.req.ThreadTS, Before: job.req.Text, DryRun: job.req.DryRun, QueuedMS: began.Sub(job.received).Milliseconds(), Score: &job.score, Words: job.req.Words}
	defer func() {
		out.TotalMS = time.Since(job.received).Milliseconds()
		if !job.req.DryRun {
			v.record(out)
		}
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
	units, edits, jevBegan, key := voiceUnits(formatted), make(chan voiceEdits, 1), time.Now(), job.req.ChannelID+"/"+job.req.ThreadTS
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
	if job.req.DryRun && landed == nil {
		e := <-edits
		out.After, out.Cut, out.Scores, out.Repeat, out.Jev, out.JevMS = voiceApplied(units, e), e.cut, e.scores, e.repeat, "late: "+cmp.Or(e.note, "answered"), time.Since(jevBegan).Milliseconds()
	}
	if job.req.DryRun {
		return out
	}
	postBegan := time.Now()
	postCtx, cancel := context.WithTimeout(ctx, voicePostLimit)
	defer cancel()
	id.Agent, id.Drive, out.PRs, out.About, out.Options = "claude", job.req.Drive, job.req.PRs, job.req.About, job.req.Options
	if job.req.EditTS != "" {
		return v.editPost(postCtx, id, job, out, units, landed, edits, jevBegan, postBegan, key)
	}
	if len(job.req.Options) > 0 {
		return v.postQuestion(postCtx, id, job, out, units, landed, edits, jevBegan, postBegan, key)
	}
	if landed != nil && last.ts != "" && landed.repeat >= voiceRepeatAt {
		return v.editRepeat(postCtx, id, job, out, last, postBegan, key)
	}
	res, err := v.app.send(postCtx, v.app.poller.plane.DB(), id, job.req.ChannelID, job.req.ThreadTS, out.After, nil, job.req.Files)
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

// editRepeat rewords the thread's last post in place when Jev reads the new one as its repeat, so he sees one post, not two.
func (v *voiceSender) editRepeat(ctx context.Context, id identity, job voiceJob, out voiceOutcome, last voiceLast, postBegan time.Time, key string) voiceOutcome {
	_, _, _, err := v.app.newAPI(id.Token).UpdateMessageContext(ctx, job.req.ChannelID, last.ts, textOption(out.After))
	out.PostMS, out.Edited, out.Permalink, out.TS = time.Since(postBegan).Milliseconds(), err == nil, id.permalink(job.req.ChannelID, last.ts, job.req.ThreadTS), last.ts
	v.posted(key, voiceLast{ts: last.ts, text: out.After, at: job.received})
	if err != nil {
		out.Error = err.Error()
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
	out.Sent = out.sent()
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
	text = voicePRLink.ReplaceAllStringFunc(doubledLink.ReplaceAllString(strings.TrimSpace(text), "$1"), func(link string) string {
		m := voicePRLink.FindStringSubmatch(link)
		return "<" + m[1] + "|" + prWords(m[2], m[3]) + ">"
	})
	text = barePRLink.ReplaceAllString(text, "$1<$2|#$3>")
	return spaceBeforeStop.ReplaceAllString(slackLinks(text), "$1$2")
}

func prWords(number, words string) string {
	words = strings.TrimSpace(words)
	if own, ok := strings.CutPrefix(words, "#"+number); ok {
		return strings.Join(append([]string{"#" + number}, strings.Fields(own)...), " ")
	}
	if fields := strings.Fields(words); len(fields) > 0 && !strings.HasPrefix(words, "http") {
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
	if e.cut = leaned(ids, post, e.cut); len(e.cut) == len(ids) {
		e.cut = e.cut[1:]
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

func readVoiceLog(path string, last int, all bool) ([]voiceOutcome, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the daemon's own voice log
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var rows []voiceOutcome
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scan.Scan() {
		var o voiceOutcome
		if err := json.Unmarshal(scan.Bytes(), &o); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if all || o.sent() {
			rows = append(rows, o)
		}
	}
	if err := scan.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return rows[max(0, len(rows)-last):], nil
}

// sent is whether the row is a message Slack holds: a post or edit that landed, never a dry run, a replay, a refusal, a settle or tap row, or a send that failed.
// unsent is a lane's post the voice took with a 202 that never reached Slack: its send failed or the sender dropped it after queuing, so the lane believes he read what he never saw.
func (o voiceOutcome) unsent() bool {
	return o.Lane != "" && !o.DryRun && o.Permalink == "" && (o.Error != "" || o.Dropped != "" && !strings.HasPrefix(o.Dropped, "refused: ") && !strings.HasPrefix(o.Dropped, "warned"))
}

func (o voiceOutcome) sent() bool {
	return !o.DryRun && o.Permalink != "" && o.Error == "" && o.Dropped == "" && o.Lane != ""
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
		if strings.TrimSpace(option) == "" {
			return errors.New("refused, not posted: a button has no words")
		}
		key := strings.ToLower(strings.TrimSpace(option))
		if seen[key] {
			return fmt.Errorf("refused, not posted: two buttons read %q; each button must read differently at a glance", option)
		}
		seen[key] = true
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
	kept := keptOpener(units, *landed)
	out.After, out.Cut = voiceApplied(units, kept), kept.cut
	res, err := v.app.send(ctx, v.app.poller.plane.DB(), id, job.req.ChannelID, job.req.ThreadTS, out.After, voiceCarded(voiceQuestionBlocks(out.After, job.req.Options), job.req.Card), job.req.Files)
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

const voiceGap = "warning, posted: it names work nobody is doing yet. A gap you name is yours: say who has it and when he hears, and record it with `lane done-check add \"<what done means>\"` (his words: \"how can you say this new version is working if its been 6 hours and those two still aren't assigned?\")"

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
func (v *voiceSender) held(ctx context.Context, req voiceRequest) (string, []string, string, float64) {
	score := 1.0
	why, attached, warning := v.judge(ctx, req, &score)
	return why, attached, warning, score
}

func (v *voiceSender) judge(ctx context.Context, req voiceRequest, score *float64) (string, []string, string) {
	numbers := voicePRNumbers(req.PRs, req.Text)
	if nestedLink.MatchString(req.Text) {
		return voiceNestedLink, nil, ""
	}
	if len(req.Options) > 0 {
		if why := v.sameCard(req, numbers); why != "" {
			return why, nil, ""
		}
	}
	questions := map[string]spawnllm.Question{"lead": spawnllm.Binary{
		Instructions: "Is the first line of `post` a bare label or heading that tells him nothing by itself, such as \"Release 3 status.\" or \"Update:\", where the answer or the news should lead?",
		Yes:          "It opens on a bare label",
		No:           "It opens on the answer or the news",
	}}
	cards := v.openCards(req.Lane, req.EditTS)
	post := map[string]string{"post": req.Text}
	if labels := voiceLabels(req.Text); len(labels) > 0 {
		post["labels"] = strings.Join(labels, "\n")
		questions["labels"] = spawnllm.Binary{
			Instructions: "Is any line of `labels`, each a PR's number and the words that name it in `post`, anything but a few plain words saying what that PR does: a cut piece of its body or of a sentence (\"#57192 Old tree only. Your words\", \"#57277 In go/sand/next\"), a lone verb or word that names nothing (\"#56958 get\", \"#56696 Port\"), run-together or mangled words (\"#55959 atablemovedeletesandcopies\"), or markup, a path or a command?",
			Yes:          "A label is a body fragment or unreadable",
			No:           "Every label is a few plain words naming what its PR does",
		}
	}
	for n, card := range cards {
		key := fmt.Sprintf("card%d", n)
		post[key] = card.After
		questions["answers_"+key] = spawnllm.Binary{
			Instructions: "Does `post` give, as settled, the answer to the question his still-open card `" + key + "` asks him, deciding for him what the card leaves to his tap?",
			Yes:          "It answers his open card for him",
			No:           "It leaves the card's question to him, or is about something else",
		}
	}
	if len(req.Options) > 0 {
		post["buttons"] = strings.Join(req.Options, "\n")
		questions["buttons"] = spawnllm.Binary{
			Instructions: "Slack shows a button's label in about 30 characters and cuts the rest. Would any line of `buttons`, each one button's label under `post`, be cut off, or fail to say by itself what tapping it does, such as \"Shimmer only on a\" or \"Pick\"?",
			Yes:          "A button is cut off or unclear",
			No:           "Every button reads whole and says what it does",
		}
	}
	for key, question := range map[string]spawnllm.Question{"correction": spawnllm.Binary{
		Instructions: "Does `post` correct, take back or apologize for something an earlier post said, such as \"Correction: four are going in\", \"I was wrong at 3:23 PM to say …\" or \"sorry, …\"?",
		Yes:          "It corrects an earlier post",
		No:           "It corrects nothing",
	}, "overwhelming": spawnllm.Binary{
		Instructions: "His words: \"less overwhelming on the screen\". Is `post` more than he can take in at a glance on one screen, such as a long list of PRs or items he must read through one by one, where one line per stack or the one thing ready now would do?",
		Yes:          "It overwhelms the screen",
		No:           "He takes it in at a glance",
	}, "plumbing": spawnllm.Binary{
		Instructions: "Does `post` tell him about the drive's own machinery rather than his work: its builders, testers, lanes or meta lane, the card or landing tool, a landing pass, sessions old or new, the old drive, team-lead, or an internal host or tool name (such as agenthost) left unexplained, such as \"A builder is fixing it now\" or \"the landing tool hasn't cleared it\"?",
		Yes:          "It talks about the drive's plumbing",
		No:           "It talks only about his work",
	}, "gap": spawnllm.Binary{
		Instructions: "Does `post` name work that nobody is doing yet, such as \"no job carries these yet\", \"nothing picked them up\" or \"still unassigned\", without saying who now has it and when he hears?",
		Yes:          "It announces a gap and leaves it unowned",
		No:           "It names no gap, or says who has it and when he hears",
	}, "miscount": spawnllm.Binary{
		Instructions: "Does `post` give a number for a set of things (\"all six\", \"three PRs\", \"both\") and then list members of that set, where the number of members it lists differs from the number it gives?",
		Yes:          "The count and the listed members disagree",
		No:           "It counts nothing it lists, or the count matches the members it lists",
	}} {
		questions[key] = question
	}
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
	budget := voiceJevBudget
	if req.DryRun {
		budget = voiceJevLate
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var ruled *voiceRuling
	checked := make(chan struct{})
	go func() {
		defer close(checked)
		ruled, _ = v.ruledBy(ctx, req.Text, req.Options, req.Rulings, budget)
	}()
	decision, err := v.decide(ctx, voiceState{Post: post, Order: slices.Sorted(maps.Keys(post))}, questions, spawnllm.DecideOpts{Timeout: budget})
	<-checked
	if ruled != nil {
		return fmt.Sprintf("refused, not posted: his %s ruling already answers this question (%q), so asking it again re-asks what he decided. Act on the ruling and say what you did", ruled.At, ruled.Binds), nil, ""
	}
	if err != nil {
		return "", nil, ""
	}
	for _, answer := range decision.Answers {
		if binary, ok := answer.(spawnllm.BinaryAnswer); ok {
			*score = min(*score, 1-binary.PYes)
		}
	}
	return v.verdict(req, cards, numbers, func(key string) bool {
		answer, ok := decision.Answers[key].(spawnllm.BinaryAnswer)
		return ok && answer.PYes >= voiceHoldAt
	})
}

// verdict turns Jev's yes answers into the post's refusal, the buttons it attaches and its warnings, the first refusal in order winning.
func (v *voiceSender) verdict(req voiceRequest, cards []voiceOutcome, numbers []int, yes func(string) bool) (string, []string, string) {
	var warnings []string
	if yes("gap") {
		warnings = append(warnings, voiceGap)
	}
	for n, card := range cards {
		if yes(fmt.Sprintf("answers_card%d", n)) {
			warnings = append(warnings, fmt.Sprintf("warning, posted: it answers your still-open card %s for him. If he decided, his tap shows on the card; if a ruling of his decides it, retire the card with lane say --edit %s naming that ruling, and never leave a card open on a question you settled", card.Permalink, card.TS))
		}
	}
	warning := strings.Join(warnings, "; ")
	if yes("lead") {
		return voiceBareLead, nil, warning
	}
	var attached []string
	if yes("approve") {
		var why string
		if why, attached = voiceApproveAsk(req); why != "" {
			return why, nil, warning
		}
		if len(attached) > 0 {
			if why := v.sameCard(req, numbers); why != "" {
				return why, nil, warning
			}
		}
	}
	if yes("typed") && len(attached) == 0 {
		return voiceTypedChoice, nil, warning
	}
	if yes("miscount") {
		return voiceMiscount, nil, warning
	}
	if yes("correction") && req.EditTS == "" {
		return v.correction(req.Lane), nil, warning
	}
	for _, check := range []struct{ key, why string }{{"plumbing", voicePlumbing}, {"labels", voiceFragmentLabel}, {"buttons", voiceUnclearButton}, {"overwhelming", voiceOverwhelming}} {
		if yes(check.key) {
			return check.why, nil, warning
		}
	}
	return "", attached, warning
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
	open := slices.DeleteFunc(slices.Clone(req.PRs), func(pr voicePR) bool { return pr.Approved })
	if len(open) > voiceMaxApprovePRs {
		return fmt.Sprintf("refused, not posted: it asks his Approve on %d PRs with no buttons. Post it with --options naming what each button does", len(open)), nil
	}
	var options []string
	for _, pr := range open {
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
		blocks = voiceCarded(voiceQuestionBlocks(out.After, job.req.Options), job.req.Card)
	}
	err := updateMessage(ctx, v.app.newAPI(id.Token), job.req.ChannelID, job.req.EditTS, out.After, blocks)
	out.PostMS, out.Edited = time.Since(postBegan).Milliseconds(), err == nil
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.TS, out.Permalink = job.req.EditTS, id.permalink(job.req.ChannelID, job.req.EditTS, job.req.ThreadTS)
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
	defer func() { _ = f.Close() }()
	posts, unsent, taps, asks := []voiceOutcome{}, []voiceOutcome{}, map[string]string{}, map[string][]map[string]string{}
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
		if out.unsent() && !out.At.Before(since) {
			unsent = append(unsent, out)
			continue
		}
		if out.DryRun || out.Permalink == "" || out.At.Before(since) || strings.HasSuffix(out.ID, "-late") {
			continue
		}
		out.TS = cmp.Or(out.TS, permalinkTS(out.Permalink))
		out.Question = len(out.Options) > 0 || v.question(out.TS)
		posts = append(posts, out)
	}
	for n := range posts {
		posts[n].About = append(posts[n].About, asks[posts[n].TS]...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"posts": posts, "unsent": unsent, "taps": taps})
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
	DryRun        bool   `json:"dry_run,omitempty"`
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
	if s.DryRun {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"permalink": "", "verdict": "ok"})
		return
	}
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
	ThreadTS  string   `json:"thread_ts,omitempty"`
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
	ctx, cancel := context.WithTimeout(r.Context(), voicePostLimit)
	defer cancel()
	api, ref := v.app.newAPI(id.Token), slack.NewRefToMessage(s.ChannelID, s.TS)
	if !slices.Contains(id.operatorChannelIDs(), s.ChannelID) {
		m, err := messageAt(ctx, api, s.ChannelID, cmp.Or(s.ThreadTS, s.TS), s.TS)
		if err != nil || m.User != id.UserID {
			http.Error(w, fmt.Sprintf("%s is not one of the operator's channels, and %s there is not a message of his the bot can read (%v)", s.ChannelID, s.TS, err), http.StatusForbidden)
			return
		}
	}
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
	defer func() { _ = f.Close() }()
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

// warnedBefore reads whether the same lane was refused for the same reason in the last voiceWarnFor, and notes this refusal: his rule that tooling blocks at most once, then warns ("I thought we made it so this can only blcokk once then it warns? its not acceptable for tooling to get in the way like this"), so the second try posts with a warning logged.
func (v *voiceSender) warnedBefore(lane, why string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	key, now := lane+"\x00"+why, time.Now()
	if at, ok := v.warned[key]; ok && now.Sub(at) < voiceWarnFor {
		delete(v.warned, key)
		return true
	}
	v.warned[key] = now
	return false
}

type voiceStatus struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	Status    string `json:"status"`
}

// serveStatus sets the working status under his thread, where a status update belongs rather than a whole message ("these kind of status iupdates should just be in the spinner/loader slack agent thing"); an empty status clears it. His DM takes the loader line with its words; a channel thread takes the agent session's processing status.
func (v *voiceSender) serveStatus(w http.ResponseWriter, r *http.Request) {
	var s voiceStatus
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&s); err != nil || s.ChannelID == "" || !slackTSPattern.MatchString(s.ThreadTS) {
		http.Error(w, "voice status needs channel_id and thread_ts", http.StatusBadRequest)
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
	if s.ChannelID == id.dm() {
		if failed := setWorkingLine(ctx, api, id, s.ChannelID, s.ThreadTS, s.Status, nil); failed != "" {
			http.Error(w, "Slack drew no loader: "+failed, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := agentSessionStatus(ctx, api, s.ChannelID, s.ThreadTS, s.Status); err != nil {
		http.Error(w, "Slack drew no working status: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// backfill links, every voiceBackfillEvery, each question the voice posted in the last day that was never linked to the earlier posts in its thread asking the same question, a card from before its links were made included: one whose buttons Slack still shows, so old threads settle on his tap as new ones do.
func (v *voiceSender) backfill(ctx context.Context) {
	for {
		v.backfillOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(voiceBackfillEvery):
		}
	}
}

func (v *voiceSender) backfillOnce(ctx context.Context) {
	id, err := loadIdentity(ctx, v.app.poller.plane.DB())
	if err != nil {
		return
	}
	api := v.app.newAPI(id.Token)
	for _, post := range v.recentPosts(time.Now().Add(-24 * time.Hour)) {
		v.mu.Lock()
		seen := v.looked[post.TS]
		v.looked[post.TS] = true
		v.mu.Unlock()
		if seen {
			continue
		}
		if len(post.Options) == 0 {
			m, err := messageAt(ctx, api, post.ChannelID, cmp.Or(post.ThreadTS, post.TS), post.TS)
			if err != nil || !slices.ContainsFunc(m.Blocks.BlockSet, func(b slack.Block) bool { return b.BlockType() == slack.MBTAction }) {
				continue
			}
			v.mu.Lock()
			v.questions[post.TS] = true
			v.mu.Unlock()
		}
		v.linkEarlier(ctx, voiceJob{req: voiceRequest{Lane: post.Lane, ChannelID: post.ChannelID, ThreadTS: post.ThreadTS}}, post.TS, post.After)
	}
}

func (v *voiceSender) recentPosts(since time.Time) []voiceOutcome {
	f, err := os.Open(v.logTo)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var posts []voiceOutcome
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for scan.Scan() {
		var out voiceOutcome
		if json.Unmarshal(scan.Bytes(), &out) != nil || out.DryRun || out.Permalink == "" || out.Lane == "" || out.At.Before(since) {
			continue
		}
		out.TS = cmp.Or(out.TS, permalinkTS(out.Permalink))
		posts = append(posts, out)
	}
	return posts
}

// keptOpener is Jev's edits to a question with its first spoken unit never cut, the question his tap names.
func keptOpener(units []voiceUnit, e voiceEdits) voiceEdits {
	ids, _ := spokenUnits(units)
	e.cut = slices.DeleteFunc(slices.Clone(e.cut), func(id string) bool { return len(ids) > 0 && id == ids[0] })
	return e
}

const voiceBareLead = "refused, not posted: it opens on a bare label, such as \"Release 3 status.\". Lead with the answer or the news itself, such as \"Release 3's staging test hasn't run: …\""

// openCards are the lane's questions of the last day he has not tapped, the one an edit rewrites aside, so a post that settles one for him is flagged.
func (v *voiceSender) openCards(lane, editing string) []voiceOutcome {
	if lane == "" {
		return nil
	}
	tapped := map[string]bool{}
	var cards []voiceOutcome
	for _, out := range v.logged(time.Now().Add(-24 * time.Hour)) {
		switch {
		case strings.HasSuffix(out.ID, "-tapped"):
			tapped[out.TS] = true
		case !out.DryRun && out.Lane == lane && out.Permalink != "" && (len(out.Options) > 0 || v.question(cmp.Or(out.TS, permalinkTS(out.Permalink)))):
			out.TS = cmp.Or(out.TS, permalinkTS(out.Permalink))
			cards = append(cards, out)
		}
	}
	cards = slices.DeleteFunc(cards, func(card voiceOutcome) bool { return tapped[card.TS] || card.TS == editing })
	if len(cards) > voiceOpenCards {
		cards = cards[len(cards)-voiceOpenCards:]
	}
	return cards
}

func (v *voiceSender) question(ts string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.questions[ts]
}

// logged is every row of the voice log since a moment, posts, taps and links alike.
func (v *voiceSender) logged(since time.Time) []voiceOutcome {
	f, err := os.Open(v.logTo)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var rows []voiceOutcome
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for scan.Scan() {
		var out voiceOutcome
		if json.Unmarshal(scan.Bytes(), &out) == nil && !out.At.Before(since) {
			rows = append(rows, out)
		}
	}
	return rows
}

const voiceCardBlock = "voice-options"

// voiceCarded names the card a question belongs to in its buttons' block id, so a tap reaches the lane with the card's id while the id never shows in his post.
func voiceCarded(blocks []slack.Block, card string) []slack.Block {
	if card == "" {
		return blocks
	}
	for _, b := range blocks {
		if actions, ok := b.(*slack.ActionBlock); ok {
			actions.BlockID = voiceCardBlock + ":" + card
		}
	}
	return blocks
}

func voiceCardOf(blocks []slack.Block) string {
	for _, b := range blocks {
		if actions, ok := b.(*slack.ActionBlock); ok {
			if card, found := strings.CutPrefix(actions.BlockID, voiceCardBlock+":"); found {
				return card
			}
		}
	}
	return ""
}

type voiceRuling struct {
	At    string `json:"at"`
	Binds string `json:"binds"`
}

// ruledBy asks Jev which of his rulings, if any, already answers a question and which option it picks, so a card never re-asks what he decided ("never re-ask what he decided"). It answers the ruling and the option, or nothing.
func (v *voiceSender) ruledBy(ctx context.Context, question string, options []string, rulings []voiceRuling, timeout time.Duration) (*voiceRuling, string) {
	if len(rulings) == 0 || len(options) == 0 {
		return nil, ""
	}
	which := []spawnllm.Option{{Value: "none", Description: "No ruling of his answers this question; it is still his to decide"}}
	for n, r := range rulings {
		which = append(which, spawnllm.Option{Value: fmt.Sprintf("r%d", n), Description: r.Binds})
	}
	picks := make([]spawnllm.Option, 0, len(options))
	for _, option := range options {
		picks = append(picks, spawnllm.Option{Value: option, Description: option})
	}
	questions := map[string]spawnllm.Question{
		"ruling": spawnllm.Label{Instructions: "Which of his rulings, each listed as an option, already answers the question `post` asks him, so that asking it again re-asks what he decided? Pick none unless one plainly settles it.", Options: which},
		"pick":   spawnllm.Label{Instructions: "If a ruling of his answers the question `post` asks, which of the post's choices does that ruling pick?", Options: picks},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	decision, err := v.decide(ctx, voiceState{Post: map[string]string{"post": question}, Order: []string{"post"}}, questions, spawnllm.DecideOpts{Timeout: timeout})
	if err != nil {
		return nil, ""
	}
	ruling, ok := decision.Answers["ruling"].(spawnllm.LabelAnswer)
	if !ok || ruling.Choice == "none" || ruling.Probabilities[ruling.Choice] < voiceHoldAt {
		return nil, ""
	}
	var n int
	if _, err := fmt.Sscanf(ruling.Choice, "r%d", &n); err != nil || n >= len(rulings) {
		return nil, ""
	}
	pick, _ := decision.Answers["pick"].(spawnllm.LabelAnswer)
	return &rulings[n], pick.Choice
}

type voiceRuled struct {
	ChannelID string        `json:"channel_id"`
	TS        string        `json:"ts"`
	ThreadTS  string        `json:"thread_ts,omitempty"`
	Question  string        `json:"question"`
	Options   []string      `json:"options"`
	Rulings   []voiceRuling `json:"rulings"`
	DryRun    bool          `json:"dry_run,omitempty"`
}

// serveRuled retires an open question one of his rulings already answers: Jev names the ruling and the option it picks, the card takes "✅ settled by your <ruling>: <pick>" with its buttons off, and the pick is logged as his answer so the posts linked to the card settle with it. The line it wrote, or none.
func (v *voiceSender) serveRuled(w http.ResponseWriter, r *http.Request) {
	var s voiceRuled
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, voiceBodyBytes)).Decode(&s); err != nil || !slackTSPattern.MatchString(s.TS) {
		http.Error(w, "voice ruled needs ts, question, options and rulings", http.StatusBadRequest)
		return
	}
	id, err := loadIdentity(r.Context(), v.app.poller.plane.DB())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	api := v.app.newAPI(id.Token)
	ctx, cancel := context.WithTimeout(r.Context(), 2*voiceJevLate)
	defer cancel()
	m, err := messageAt(ctx, api, s.ChannelID, cmp.Or(s.ThreadTS, s.TS), s.TS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	options := s.Options
	if len(options) == 0 {
		options = voiceButtonsOf(m.Blocks.BlockSet)
	}
	var ruling *voiceRuling
	var pick, line string
	if len(options) > 0 {
		ruling, pick = v.ruledBy(ctx, cmp.Or(s.Question, m.Text), options, s.Rulings, voiceJevLate)
	}
	if ruling != nil && pick != "" {
		line = fmt.Sprintf("✅ settled by your %s rule: %s", ruling.At, pick)
	}
	if line != "" && !s.DryRun {
		if err := updateMessage(ctx, api, s.ChannelID, s.TS, m.Text, voiceSettled(m.Blocks.BlockSet, m.Text, line, false)); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := voiceTapped(s.ChannelID, s.TS, pick); err != nil {
			v.log.Printf("voice ruled %s: pick not logged: %v", s.TS, err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"line": line, "verdict": "ok"})
}

func voiceButtonsOf(blocks []slack.Block) []string {
	var labels []string
	for _, b := range blocks {
		if actions, ok := b.(*slack.ActionBlock); ok {
			for _, e := range actions.Elements.ElementSet {
				if button, ok := e.(*slack.ButtonBlockElement); ok && strings.HasPrefix(button.ActionID, voiceOptionAction) {
					labels = append(labels, button.Value)
				}
			}
		}
	}
	return labels
}

const (
	voiceSameCardFor   = time.Hour
	voiceOverwhelming  = "refused, not posted: it is more than he can take in on one screen (his words: \"less overwhelming on the screen\", \"I have not asked for 900 or 300 things theres a few lanes of well designed work\"). Ask about the one thing ready now, one line per stack, and say the rest when each is"
	voiceUnclearButton = "refused, not posted: a button would not read whole or say by itself what tapping it does (his words: \"the buttons are all cut off I have no idea what they're saying\"). Write each option as a short label he reads at a glance, never a cut-off phrase"
	voiceNestedLink    = "refused, not posted: it puts a link inside another link's label, which Slack draws as markup he can't read. Name each PR once, as #N plus at most four words of what it does"
	voiceFragmentLabel = "refused, not posted: a PR label is a piece of its body or unreadable words. Name each PR as #N plus at most four plain words of what it does ({pr:N|the words}), then post it again"
)

var nestedLink = regexp.MustCompile(`<https?://[^<>|\s]+\|[^<>]*(?:<|\[[^\]]*\]\()|\[[^\]]*\]\(<`)

func voicePRNumbers(prs []voicePR, text string) []int {
	var numbers []int
	for _, pr := range prs {
		numbers = append(numbers, pr.Number)
	}
	for _, m := range voicePRLink.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			numbers = append(numbers, n)
		}
	}
	for _, m := range barePRLink.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[3]); err == nil {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	return slices.Compact(numbers)
}

func voiceLabels(text string) []string {
	var labels []string
	for _, m := range voicePRLink.FindAllStringSubmatch(text, -1) {
		if label := strings.TrimSpace(m[3]); label != "" && label != "#"+m[2] {
			labels = append(labels, label)
		}
	}
	return labels
}

// sameCard refuses a lane's second card within the hour on a PR its first card already asks about: the first card is edited, never asked again.
func (v *voiceSender) sameCard(req voiceRequest, numbers []int) string {
	if req.Lane == "" || len(numbers) == 0 {
		return ""
	}
	for _, out := range slices.Backward(v.logged(time.Now().Add(-voiceSameCardFor))) {
		ts := cmp.Or(out.TS, permalinkTS(out.Permalink))
		if out.DryRun || out.Lane != req.Lane || out.Permalink == "" || len(out.Options) == 0 || ts == req.EditTS {
			continue
		}
		var shared []string
		for _, n := range voicePRNumbers(out.PRs, out.After) {
			if _, ok := slices.BinarySearch(numbers, n); ok {
				shared = append(shared, fmt.Sprintf("#%d", n))
			}
		}
		if len(shared) > 0 {
			return fmt.Sprintf("refused, not posted: your card %s already asks him about %s, posted %s ago. Edit that card with lane say --edit %s, never post a second one on the same PRs", out.Permalink, strings.Join(shared, ", "), time.Since(out.At).Round(time.Minute), ts)
		}
	}
	return ""
}

const (
	voiceCorrection = "refused, not posted: it corrects an earlier post as a new one. A correction rewords the post it corrects in place, with lane say --edit <its ts>, and says no sorry"
	voicePlumbing   = "refused, not posted: it tells him about the drive's own machinery (builders, lanes, tools, passes, sessions). Say what happened to his work and when he hears next, never who in the drive is doing it"
)

// retried lets a lane's second try past a refusal it met within voiceWarnFor, his rule that tooling blocks at most once: a correction becomes an edit of the post it corrects, since that fix is known, and any other refusal posts with a warning logged.
func (v *voiceSender) retried(req *voiceRequest, why string) string {
	if why == "" || req.DryRun || !v.warnedBefore(req.Lane, why) {
		return why
	}
	if ts := v.corrected(*req); strings.HasPrefix(why, voiceCorrection) && ts != "" {
		req.EditTS = ts
		return ""
	}
	v.record(voiceOutcome{ID: voiceID() + "-warned", At: time.Now(), Lane: req.Lane, ChannelID: req.ChannelID, ThreadTS: req.ThreadTS, Before: req.Text, Formatted: req.Text, Dropped: "warned, posted anyway: " + why, DryRun: true})
	return ""
}

// editAt names the post an edit rewrites by its Slack ts: a lane that names its earlier post by the voice id its say returned, as new-tree-cutover did at 8:26 PM with 9fa27f1881f3, gets that post's ts from the log, and an edit naming neither is refused before it is queued, so no edit is logged against a ts Slack never gave.
func (v *voiceSender) editAt(req *voiceRequest) error {
	if req.EditTS == "" || slackTSPattern.MatchString(req.EditTS) {
		return nil
	}
	for _, out := range slices.Backward(v.logged(time.Now().Add(-voiceEditFor))) {
		if out.ID == req.EditTS && out.sent() && out.TS != "" {
			req.EditTS = out.TS
			return nil
		}
	}
	return fmt.Errorf("edit_ts %q is no Slack ts and names no post the voice sent in the last %s: name the post by its ts or permalink", req.EditTS, voiceEditFor)
}

// corrected names the lane's latest post in the same thread within six hours, the one a correction sent twice edits in place: the second try's fix is known, so it is applied rather than posted beside the post it corrects.
func (v *voiceSender) corrected(req voiceRequest) string {
	for _, out := range slices.Backward(v.logged(time.Now().Add(-6 * time.Hour))) {
		if out.Lane == req.Lane && out.sent() && out.ChannelID == req.ChannelID && out.ThreadTS == req.ThreadTS {
			return cmp.Or(out.TS, permalinkTS(out.Permalink))
		}
	}
	return ""
}

func (v *voiceSender) correction(lane string) string {
	var recent []string
	for _, out := range slices.Backward(v.logged(time.Now().Add(-6 * time.Hour))) {
		if out.Lane == lane && out.sent() && len(recent) < 3 {
			recent = append(recent, fmt.Sprintf("%s (%s)", cmp.Or(out.TS, permalinkTS(out.Permalink)), out.At.Local().Format(time.Kitchen)))
		}
	}
	if len(recent) == 0 {
		return voiceCorrection
	}
	return voiceCorrection + ". Your latest posts: " + strings.Join(recent, ", ")
}
