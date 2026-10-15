package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
	"offgrid/internal/structured"
)

// Route is what the router decided.
type Route string

const (
	RouteLocal    Route = "local"    // answered now by the local agent (rules or small model)
	RouteCloud    Route = "cloud"    // answered now by a cloud model (only when online and one exists)
	RouteDeferred Route = "deferred" // saved in the durable queue for later
	RouteStatus   Route = "status"   // a question about the queue itself
	RouteFailed   Route = "failed"   // the message needed deferring and the queue refused it
)

// Job kinds the router creates. A later worker (Day 12) will pick them up.
const (
	// KindDeepThink: the request asks for judgement or analysis.
	KindDeepThink = "deep_think"
	// KindInterpret: the local model could not read the message, so a
	// stronger model should try later.
	KindInterpret = "interpret"
)

// Queue is the part of the job queue the router uses. *jobqueue.Queue
// satisfies it, and tests can pass a fake.
type Queue interface {
	Enqueue(ctx context.Context, key, kind string, payload json.RawMessage, opt jobqueue.EnqueueOptions) (jobqueue.Job, bool, error)
	Stats(ctx context.Context) (jobqueue.Stats, error)
	List(ctx context.Context, status jobqueue.Status, limit int) ([]jobqueue.Job, error)
}

// Cloud is an optional bigger model that can answer hard requests directly
// while the network is up. Nothing in the Offgrid CLI provides one yet: the
// hook exists so that "online" can change the decision, and so the rule is
// tested before any real cloud client is written.
type Cloud interface {
	Answer(ctx context.Context, text string) (string, error)
}

// Router wraps the local agent with the answer-now-or-defer decision.
type Router struct {
	Agent *agent.Agent
	Queue Queue
	// Net reports connectivity. nil, or Unknown, is treated as "not online":
	// never claim a connection that has not been seen.
	Net   func() connectivity.State
	Cloud Cloud            // optional
	Now   func() time.Time // nil means time.Now
}

// Result is everything the router did with one message.
type Result struct {
	Text    string
	Route   Route
	Reason  string // why, in a phrase
	Net     connectivity.State
	Outcome agent.Outcome // the local agent's outcome, when it ran

	Job     *jobqueue.Job // when deferred
	Created bool          // false: the same request was already queued
	Answer  string        // when answered by the cloud
	Err     error         // when Route is failed
	Message string        // what to show the person
	Total   time.Duration
}

// Handle decides and acts. Like Agent.Handle it never returns an error:
// whatever happened is in the Result.
func (r *Router) Handle(ctx context.Context, text string) Result {
	now := r.now
	start := now()
	state := connectivity.Unknown
	if r.Net != nil {
		state = r.Net()
	}
	res := Result{Text: text, Net: state}
	// finish stamps the elapsed time on the result it is given. It takes the
	// result as an argument because helpers below receive Result by value:
	// a closure over the outer variable would return stale data.
	finish := func(r Result) Result {
		r.Total = now().Sub(start)
		return r
	}
	text = strings.Join(strings.Fields(text), " ")

	// 0. A question about the queue is answered from the queue, instantly.
	if isQueueQuestion(text) {
		res.Route, res.Reason = RouteStatus, "a question about the queue"
		res.Message = r.queueSummary(ctx)
		return finish(res)
	}

	// 1. Rules first, exactly as in Day 5. A message a rule fully understands
	// is simple by definition, even if its title contains a word like
	// "suggest": "new task: suggest a supplier" must create a task, not queue.
	if _, _, ok := r.Agent.Rules.Match(text); ok {
		return r.local(ctx, text, res, "a rule understood it", finish)
	}

	// 2. Complexity gate.
	if c := Assess(text); c.Hard {
		return r.hard(ctx, text, c, res, finish)
	}

	// 3. Simple enough to try locally; then check output validity.
	out := r.Agent.Handle(ctx, text)
	res.Outcome = out
	if why, kind := escalation(ctx, out, text); why != "" {
		return r.deferTo(ctx, text, kind, why, res, finish)
	}
	res.Route, res.Reason, res.Message = RouteLocal, "the local agent handled it", out.Message()
	return finish(res)
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) local(ctx context.Context, text string, res Result, reason string, finish func(Result) Result) Result {
	out := r.Agent.Handle(ctx, text)
	res.Outcome = out
	if why, kind := escalation(ctx, out, text); why != "" {
		return r.deferTo(ctx, text, kind, why, res, finish)
	}
	res.Route, res.Reason, res.Message = RouteLocal, reason, out.Message()
	return finish(res)
}

// hard handles a message the gate judged to need more than the local agent.
// The only thing that can answer it now is a cloud model, and only if the
// network is up.
func (r *Router) hard(ctx context.Context, text string, c Complexity, res Result, finish func(Result) Result) Result {
	reason := c.Reason()
	if r.Cloud != nil && res.Net == connectivity.Online {
		ans, err := r.Cloud.Answer(ctx, text)
		if err == nil {
			res.Route, res.Reason, res.Answer, res.Message = RouteCloud, reason+"; online, so a cloud model answered", ans, ans
			return finish(res)
		}
		reason += fmt.Sprintf("; the cloud model failed (%v)", err)
	}
	return r.deferTo(ctx, text, KindDeepThink, reason, res, finish)
}

// escalation looks at what the local agent did and says whether the message
// should wait for a stronger model instead. It returns a reason and the job
// kind, or "" to keep the local outcome.
func escalation(ctx context.Context, o agent.Outcome, text string) (reason, kind string) {
	switch o.Status {
	case agent.StatusNotUnderstood:
		// The model tried and produced nothing valid. A word or two of noise
		// ("asdf") is not worth keeping; a real sentence is.
		if len(strings.Fields(text)) >= 3 {
			return "the local model could not turn it into a valid command", KindInterpret
		}
	case agent.StatusFailed:
		switch {
		case errors.Is(o.Err, agent.ErrNoModel):
			return "it needs the local model, and none is available", KindInterpret
		case errors.Is(o.Err, llm.ErrUnavailable):
			return "the local model is not reachable", KindInterpret
		case errors.Is(o.Err, context.DeadlineExceeded) && ctx.Err() == nil:
			return "the local model took too long", KindInterpret
		case errors.Is(o.Err, structured.ErrNoValidReply):
			return "the local model could not turn it into a valid command", KindInterpret
		}
	}
	return "", ""
}

// deferTo saves the message as a job and builds the visible status.
func (r *Router) deferTo(ctx context.Context, text, kind, reason string, res Result, finish func(Result) Result) Result {
	res.Reason = reason
	payload, _ := json.Marshal(map[string]string{"text": text})
	job, created, err := r.Queue.Enqueue(ctx, r.key(kind, text), kind, payload, jobqueue.EnqueueOptions{})
	if err != nil {
		res.Route, res.Err = RouteFailed, err
		res.Message = fmt.Sprintf("I could not do that now, and I could not save it for later either: %v", err)
		return finish(res)
	}
	res.Route, res.Job, res.Created = RouteDeferred, &job, created
	res.Message = deferredMessage(job, created, kind, reason, res.Net)
	return finish(res)
}

// key identifies "this request, today". The same sentence typed twice on the
// same day is one job (so a double-tap or a retry never queues it twice); the
// same sentence tomorrow is a new request. It is derived from the exact text,
// because the payload stored with the job must match whenever the key does.
func (r *Router) key(kind, text string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + text + "\x00" + r.now().Format("2006-01-02")))
	return "route:" + kind + ":" + hex.EncodeToString(sum[:8])
}

func deferredMessage(j jobqueue.Job, created bool, kind, reason string, net connectivity.State) string {
	var b strings.Builder
	if created {
		fmt.Fprintf(&b, "Saved for later as job #%d. ", j.ID)
	} else {
		fmt.Fprintf(&b, "Already saved as job #%d (%s). Not added again. ", j.ID, j.Status)
	}
	switch kind {
	case KindDeepThink:
		b.WriteString("This needs deeper thinking than the local model can do. ")
	default:
		b.WriteString("I could not read it with the local model now. ")
	}
	switch net {
	case connectivity.Online:
		b.WriteString("You are online, so it will run as soon as the background worker is free.")
	case connectivity.Degraded:
		b.WriteString("The connection is slow, so it will wait until it improves.")
	case connectivity.Offline:
		b.WriteString("You are offline, so it will run when the connection is back.")
	default:
		b.WriteString("The connection is not known, so it will wait until you are online.")
	}
	fmt.Fprintf(&b, " (why: %s)", reason)
	return b.String()
}

// ---- the queue question -----------------------------------------------------

// Closed list, and deliberately only phrases about the QUEUE. Words like
// "pending" or "waiting" are not here: the task rules already read them as
// questions about tasks, and the router must not steal those.
var queueQuestions = map[string]bool{
	"queue": true, "queue status": true, "the queue": true, "show queue": true, "show the queue": true,
	"whats queued": true, "what is queued": true, "whats in the queue": true, "what is in the queue": true,
	"pending jobs": true, "deferred": true, "whats deferred": true, "what is deferred": true, "deferred jobs": true,
}

func isQueueQuestion(text string) bool { return queueQuestions[normalize(text)] }

func (r *Router) queueSummary(ctx context.Context) string {
	st, err := r.Queue.Stats(ctx)
	if err != nil {
		return fmt.Sprintf("I could not read the queue: %v", err)
	}
	if st.Queued+st.Running+st.Dead == 0 {
		if st.Done > 0 {
			return fmt.Sprintf("Nothing is waiting. %d finished.", st.Done)
		}
		return "Nothing is waiting."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d waiting, %d running, %d finished, %d failed for good.", st.Queued, st.Running, st.Done, st.Dead)
	for _, status := range []jobqueue.Status{jobqueue.Running, jobqueue.Queued, jobqueue.Dead} {
		jobs, err := r.Queue.List(ctx, status, 10)
		if err != nil {
			continue
		}
		for _, j := range jobs {
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(j.Payload, &p)
			text := p.Text
			if len(text) > 60 {
				text = text[:57] + "..."
			}
			fmt.Fprintf(&b, "\n  #%d %-8s %s", j.ID, j.Status, text)
		}
	}
	return b.String()
}
