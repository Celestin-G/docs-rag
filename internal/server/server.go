package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/store"
	"golang.org/x/time/rate"
)

// rateLimit: 5 requests per minute per IP, burst of 3.
const (
	ratePerMinute = 5
	rateBurst     = 3
	cleanupEvery  = 5 * time.Minute
	expireAfter   = 10 * time.Minute
)

type limitEntry struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // unix nano
}

type srv struct {
	store    *store.Store
	emb      *embedder.Embedder
	llm      *llm.Client
	limiters sync.Map // map[string]*limitEntry

	eeMu        sync.Mutex
	eeFeatures  []string
	eeFetchedAt time.Time
}

// enterpriseFeatures returns the EE feature list for the system prompt,
// cached for 5 minutes. Returns nil on error — the prompt falls back to its
// static list, so the guard never disappears.
func (s *srv) enterpriseFeatures(ctx context.Context) []string {
	s.eeMu.Lock()
	defer s.eeMu.Unlock()
	if time.Since(s.eeFetchedAt) < 5*time.Minute && s.eeFeatures != nil {
		return s.eeFeatures
	}
	feats, err := s.store.EnterpriseFeatures(ctx)
	if err != nil {
		log.Printf("enterprise features lookup: %v", err)
		return s.eeFeatures // possibly stale, better than nothing
	}
	s.eeFeatures = feats
	s.eeFetchedAt = time.Now()
	return feats
}

// New wires up HTTP handlers and returns a ready handler.
func New(st *store.Store, emb *embedder.Embedder, lm *llm.Client) http.Handler {
	s := &srv{store: st, emb: emb, llm: lm}
	go s.cleanupLimiters()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /api/ask", s.withRateLimit(s.handleAsk))
	mux.HandleFunc("POST /api/feedback", s.withRateLimit(s.handleFeedback))
	mux.HandleFunc("GET /api/stats", s.handleStatsJSON)
	mux.HandleFunc("GET /stats", s.handleStatsPage)
	// MCP endpoint for agents. The streamable HTTP transport uses POST for calls,
	// GET for the server-to-client stream and DELETE to end a session. Each method
	// is registered on its own: a method-less "/api/mcp" pattern conflicts with
	// "GET /" above, and ServeMux panics rather than pick a winner.
	mcpHandler := s.mcpHandler()
	mux.Handle("POST /api/mcp", mcpHandler)
	mux.Handle("GET /api/mcp", mcpHandler)
	mux.Handle("DELETE /api/mcp", mcpHandler)
	return mux
}

// clientIP extracts the real client IP, honouring X-Forwarded-For when present.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	// Strip port from RemoteAddr.
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func (s *srv) getLimiter(ip string) *rate.Limiter {
	e := &limitEntry{limiter: rate.NewLimiter(rate.Every(time.Minute/ratePerMinute), rateBurst)}
	if v, loaded := s.limiters.LoadOrStore(ip, e); loaded {
		e = v.(*limitEntry)
	}
	e.lastSeen.Store(time.Now().UnixNano())
	return e.limiter
}

func (s *srv) withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !s.getLimiter(ip).Allow() {
			log.Printf("rate limit hit: %s", ip)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests — please wait a moment before asking again.", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// cleanupLimiters removes stale per-IP entries to prevent unbounded memory growth.
func (s *srv) cleanupLimiters() {
	ticker := time.NewTicker(cleanupEvery)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-expireAfter).UnixNano()
		s.limiters.Range(func(k, v any) bool {
			if v.(*limitEntry).lastSeen.Load() < cutoff {
				s.limiters.Delete(k)
			}
			return true
		})
	}
}

func (s *srv) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, ui)
}

// --- /api/ask -----------------------------------------------------------------

type askReq struct {
	Question   string        `json:"question"`
	TopK       int           `json:"top_k"`
	History    []llm.Message `json:"history"`
	Version    string        `json:"version"`    // "3.0","3.1","3.2","3.3"
	Deployment string        `json:"deployment"` // "cloud","self-hosted","both"
	License    string        `json:"license"`    // "ee","community"
}

type sseEvent struct {
	Type    string   `json:"type"`
	Text    string   `json:"text,omitempty"`
	Sources []source `json:"sources,omitempty"`
}

type source struct {
	File    string `json:"file"`
	Section string `json:"section,omitempty"`
}

// sseWriter wraps ResponseWriter to format writes as SSE data frames.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (sw *sseWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	data, _ := json.Marshal(sseEvent{Type: "chunk", Text: string(p)})
	fmt.Fprintf(sw.w, "data: %s\n\n", data)
	sw.flusher.Flush()
	return len(p), nil
}

// Input caps: protect the embedder, the LLM context window and the DB from abuse.
const (
	maxQuestionLen = 2000
	maxHistoryLen  = 12 // last 6 user/assistant turns
	maxBodyBytes   = 256 << 10
)

func (s *srv) handleAsk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req askReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Question) > maxQuestionLen {
		http.Error(w, "question too long (max 2000 characters)", http.StatusBadRequest)
		return
	}
	req.normalise()

	// Analytics: track timing and outcome for every request.
	start := time.Now()
	reqStatus := "ok"
	chunksFound := 0
	defer func() {
		go func() {
			if err := s.store.InsertRequestLog(
				context.Background(), req.Question,
				int(time.Since(start).Milliseconds()), chunksFound, reqStatus,
			); err != nil {
				log.Printf("request log insert: %v", err)
			}
		}()
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()

	send := func(ev sseEvent) {
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Shared with the MCP tool — see (*srv).answer.
	res, err := s.answer(ctx, req, &sseWriter{w: w, flusher: flusher})
	chunksFound = len(res.Sources)
	reqStatus = res.Status
	if err != nil {
		send(sseEvent{Type: "error", Text: err.Error()})
		return
	}
	if res.Status == "no_results" {
		send(sseEvent{Type: "chunk", Text: noResultsMessage})
		send(sseEvent{Type: "done"})
		return
	}

	send(sseEvent{Type: "sources", Sources: res.Sources})
	send(sseEvent{Type: "done"})
}

// --- Feedback -----------------------------------------------------------------

type feedbackReq struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Rating   int    `json:"rating"` // 1 = thumbs up, -1 = thumbs down
}

func (s *srv) handleFeedback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req feedbackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Rating != 1 && req.Rating != -1) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Question) > maxQuestionLen {
		req.Question = req.Question[:maxQuestionLen]
	}
	if len(req.Answer) > 20000 {
		req.Answer = req.Answer[:20000]
	}
	if err := s.store.InsertFeedback(r.Context(), req.Question, req.Answer, req.Rating); err != nil {
		log.Printf("feedback insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Stats --------------------------------------------------------------------

func (s *srv) handleStatsJSON(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (s *srv) handleStatsPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	positiveStr := "—"
	if st.TotalFeedback > 0 {
		positiveStr = fmt.Sprintf("%.1f%%", st.PositiveRate*100)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, statsPageHTML,
		st.TotalRequests, st.RequestsToday,
		int(st.AvgDurationMs), st.ErrorRate*100,
		st.TotalFeedback, positiveStr,
	)
}

const statsPageHTML = `<!DOCTYPE html>
<html lang="fr"><head>
<meta charset="UTF-8">
<title>Stats — Formance Docs RAG</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:'SF Mono','Fira Code',monospace;background:#01353C;color:#BAEABC;padding:48px 32px;min-height:100vh}
h1{font-size:11px;letter-spacing:.1em;text-transform:uppercase;color:#7A9396;margin-bottom:32px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:16px;max-width:680px}
.card{background:#0d4046;border:1px solid #486568;border-radius:3px;padding:20px 24px}
.card-title{font-size:10px;letter-spacing:.08em;text-transform:uppercase;color:#7A9396;margin-bottom:16px}
.stat{display:flex;justify-content:space-between;align-items:baseline;padding:6px 0;border-bottom:1px solid rgba(255,255,255,.06)}
.stat:last-child{border-bottom:none}
.stat-label{font-size:12px;color:#D5E1E1}
.stat-value{font-size:20px;font-weight:700;color:#BAEABC;letter-spacing:-.02em}
.stat-value small{font-size:11px;font-weight:400;color:#7A9396;margin-left:4px}
</style>
</head><body>
<h1>_stats/ Formance Docs RAG</h1>
<div class="grid">
  <div class="card">
    <div class="card-title">Requetes</div>
    <div class="stat"><span class="stat-label">Total</span><span class="stat-value">%d</span></div>
    <div class="stat"><span class="stat-label">Aujourd'hui</span><span class="stat-value">%d</span></div>
    <div class="stat"><span class="stat-label">Duree moyenne</span><span class="stat-value">%d<small>ms</small></span></div>
    <div class="stat"><span class="stat-label">Taux d'erreur</span><span class="stat-value">%.1f<small>%%</small></span></div>
  </div>
  <div class="card">
    <div class="card-title">Feedback</div>
    <div class="stat"><span class="stat-label">Total feedbacks</span><span class="stat-value">%d</span></div>
    <div class="stat"><span class="stat-label">Reponses positives</span><span class="stat-value">%s</span></div>
  </div>
</div>
</body></html>`

// --- Embedded UI --------------------------------------------------------------

const ui = `<!DOCTYPE html>
<html lang="fr">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Formance Docs RAG</title>
<style>
:root{
  --emerald:#01353C;--emerald-10:#486568;--emerald-20:#7A9396;
  --slate:#D5E1E1;--slate-10:#EBEEEE;--slate-20:#F2F4F4;
  --gold:#7E6F4A;--lilac:#CDCBFF;--cobalt:#397AAB;--mint:#BAEABC;--mint-600:#97C399;
  --mono:'SF Mono','Fira Code','Fira Mono','Roboto Mono',Courier,monospace;
}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;background:var(--slate);height:100vh;display:flex;flex-direction:column;overflow:hidden;color:var(--emerald)}

/* Header */
header{background:var(--emerald);height:48px;padding:0 24px;display:flex;align-items:center;gap:12px;flex-shrink:0}
.eyebrow{font-family:var(--mono);font-size:11px;font-weight:500;letter-spacing:.05em;text-transform:uppercase;background:var(--emerald-10);color:var(--slate-20);padding:3px 9px;border-radius:2px}
.wordmark{font-family:var(--mono);font-size:14px;font-weight:500;color:var(--slate);letter-spacing:.02em}
.dot{width:6px;height:6px;border-radius:50%;background:var(--mint);margin-left:auto}

/* Messages */
#messages{flex:1;overflow-y:auto;padding:32px 24px;display:flex;flex-direction:column;gap:28px}
#messages::-webkit-scrollbar{width:3px}
#messages::-webkit-scrollbar-thumb{background:var(--emerald-20);border-radius:2px}

/* Empty state */
#empty{flex:1;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:10px;text-align:center;padding-bottom:80px}
.e-eyebrow{font-family:var(--mono);font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--emerald-10)}
.e-heading{font-size:26px;font-weight:600;color:var(--emerald);line-height:1.15;letter-spacing:-.4px;max-width:420px}
.e-sub{font-family:var(--mono);font-size:12px;color:var(--emerald-10);max-width:360px;line-height:1.7;margin-top:2px}
.suggestions{display:flex;flex-wrap:wrap;gap:8px;margin-top:18px;justify-content:center}
.sug{padding:6px 14px;background:transparent;border:1px solid var(--emerald-20);border-radius:2px;font-family:var(--mono);font-size:12px;color:var(--emerald);cursor:pointer;transition:all .12s;letter-spacing:.02em}
.sug:hover{background:var(--emerald);color:var(--slate);border-color:var(--emerald)}

/* Message blocks */
.message{max-width:760px;width:100%;margin:0 auto;display:flex;flex-direction:column;gap:4px}
.role{font-family:var(--mono);font-size:10px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--emerald-20);padding:0 2px}
.role.r{text-align:right}
.row{display:flex}
.row.r{justify-content:flex-end}
.bubble{padding:12px 16px;font-size:15px;line-height:1.7;border-radius:2px}
.bubble.u{background:var(--emerald);color:var(--slate-20);max-width:78%}
.bubble.a{background:#fff;color:var(--emerald);border:1px solid var(--slate-10);word-break:break-word;width:100%}
.bubble.a h2{font-size:15px;font-weight:600;margin:10px 0 4px}
.bubble.a h3,.bubble.a h4,.bubble.a h5{font-size:14px;font-weight:600;margin:8px 0 3px}
.bubble.a p{margin:4px 0;line-height:1.7}
.bubble.a ul,.bubble.a ol{margin:4px 0 4px 18px;line-height:1.65}
.bubble.a li{margin:2px 0}
.bubble.a strong{font-weight:600}
.bubble.a em{font-style:italic}
.bubble.a code{font-family:var(--mono);font-size:12px;background:var(--slate-10);padding:1px 5px;border-radius:2px;color:var(--emerald)}
.bubble.a pre{background:var(--emerald);border-radius:3px;padding:12px 14px;margin:8px 0;overflow-x:auto}
.bubble.a pre code{background:none;color:var(--mint);padding:0;font-size:12px}
.bubble.a blockquote{border-left:3px solid var(--emerald-20);margin:6px 0;padding:2px 12px;color:var(--emerald-10)}
.tbl-wrap{overflow-x:auto;margin:8px 0}
.bubble.a table{border-collapse:collapse;width:100%;font-size:12px;font-family:var(--mono)}
.bubble.a th{background:var(--emerald);color:var(--mint);font-size:11px;font-weight:500;letter-spacing:.04em;padding:6px 12px;text-align:left;border:1px solid var(--emerald-10)}
.bubble.a td{padding:6px 12px;border:1px solid var(--slate);color:var(--emerald)}
.bubble.a tr:nth-child(even) td{background:var(--slate-20)}
/* Thinking words */
.tw{font-family:var(--mono);font-size:13px;color:var(--emerald-10);font-style:italic}
/* Send button loading dots */
#btn .ldot{display:inline-block;width:3px;height:3px;border-radius:50%;background:var(--mint);margin:0 2px;vertical-align:middle;animation:ldotWave 1.4s ease-in-out infinite}
#btn .ldot:nth-child(2){animation-delay:.2s}
#btn .ldot:nth-child(3){animation-delay:.4s}
@keyframes ldotWave{0%,25%,100%{transform:translateY(0)}12%{transform:translateY(-4px)}}

/* Gold blinking cursor */
.cursor{display:inline-block;width:8px;height:.9em;background:var(--gold);margin-left:2px;animation:blink 1s step-end infinite;vertical-align:middle}
@keyframes blink{50%{opacity:0}}

/* Source chips — terminal style */
.sources{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
.chip{font-family:var(--mono);font-size:11px;background:var(--emerald);color:var(--mint);padding:3px 10px;border-radius:2px;max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;cursor:default;letter-spacing:.02em;transition:background .12s}
.chip:hover{background:var(--emerald-10)}

/* Feedback buttons */
.fb-row{display:flex;align-items:center;gap:8px;margin-top:10px;padding-top:8px;border-top:1px solid var(--slate)}
.fb-label{font-family:var(--mono);font-size:11px;color:var(--emerald-10)}
.fb-btn{background:transparent;border:1px solid var(--emerald-20);border-radius:2px;padding:3px 10px;cursor:pointer;font-size:14px;transition:all .12s;line-height:1.4}
.fb-btn:hover{background:var(--slate-10);border-color:var(--emerald)}
.fb-thanks{font-family:var(--mono);font-size:11px;color:var(--mint-600)}

/* Input bar */
#form-wrap{background:var(--slate-10);border-top:1px solid var(--emerald-20);padding:16px 24px;flex-shrink:0;position:relative}
#form{max-width:760px;margin:0 auto;display:flex;align-items:stretch}
#fsel-group{position:absolute;left:24px;top:50%;transform:translateY(-50%);display:flex;align-items:stretch}
.fsel{height:44px;font-family:var(--mono);font-size:11px;color:var(--emerald);background:#fff;border:1px solid var(--emerald-20);border-right:none;padding:0 20px 0 10px;cursor:pointer;outline:none;letter-spacing:.02em;-webkit-appearance:none;appearance:none;background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='8' height='4' viewBox='0 0 8 4'%3E%3Cpath d='M0 0l4 4 4-4z' fill='%231a3a2a' opacity='.3'/%3E%3C/svg%3E");background-repeat:no-repeat;background-position:right 6px center;transition:border-color .12s;box-sizing:border-box}
.fsel:hover,.fsel:focus{border-color:var(--emerald)}
#sel-version{border-radius:2px 0 0 2px}
#sel-license{border-right:1px solid var(--emerald-20);border-radius:0 2px 2px 0}
.prefix{font-family:var(--mono);font-size:14px;color:var(--gold);background:var(--emerald);padding:10px 12px;border-radius:2px 0 0 2px;display:flex;align-items:center;flex-shrink:0;user-select:none;border:1px solid var(--emerald);border-right:none}
#q{flex:1;padding:10px 14px;border:1px solid var(--emerald-20);border-left:none;border-right:none;background:#fff;font-size:14px;font-family:var(--mono);resize:none;min-height:44px;max-height:160px;outline:none;line-height:1.5;color:var(--emerald);border-radius:0;transition:border-color .12s}
#q::placeholder{color:var(--emerald-20)}
#q:focus{border-color:var(--emerald)}
#btn{padding:0 20px;background:var(--emerald);color:var(--mint);border:1px solid var(--emerald);border-left:none;border-radius:0 2px 2px 0;font-family:var(--mono);font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;cursor:pointer;transition:background .12s;flex-shrink:0}
#btn:hover:not(:disabled){background:var(--emerald-10)}
#btn:disabled{opacity:.45;cursor:not-allowed}
</style>
</head>
<body>
<header>
  <span class="eyebrow">_docs/</span>
  <span class="wordmark">Formance</span>
  <span class="dot"></span>
</header>

<div id="messages">
  <div id="empty">
    <div class="e-eyebrow">_proof-of-concept/</div>
    <h2 class="e-heading">A RAG assistant over Formance's internal documentation.</h2>
    <p class="e-sub">Proof of concept — for internal use today, intended to later serve customers publicly.<br>Ask a question about the platform to get a sourced answer.</p>
    <div class="suggestions">
      <div class="sug" onclick="fill('What is Numscript?')">What is Numscript?</div>
      <div class="sug" onclick="fill('How does the Ledger module work?')">How does the Ledger work?</div>
      <div class="sug" onclick="fill('How do I configure a payment connector?')">Configure a connector</div>
      <div class="sug" onclick="fill('What are Wallets used for?')">What are Wallets?</div>
    </div>
  </div>
</div>

<div id="form-wrap">
  <div id="fsel-group">
    <select id="sel-version" class="fsel">
      <option value="3.0">v3.0</option>
      <option value="3.1">v3.1</option>
      <option value="3.2" selected>v3.2</option>
      <option value="3.3">v3.3</option>
    </select>
    <select id="sel-deploy" class="fsel">
      <option value="cloud" selected>Cloud</option>
      <option value="self-hosted">Self-hosted</option>
      <option value="both">Both</option>
    </select>
    <select id="sel-license" class="fsel">
      <option value="ee" selected>EE</option>
      <option value="community">Community</option>
    </select>
  </div>
  <form id="form">
    <span class="prefix">&gt;_</span>
    <textarea id="q" placeholder="Ask your question... (Enter to send)" rows="1"></textarea>
    <button type="submit" id="btn">Send</button>
  </form>
</div>

<script>
var q = document.getElementById('q');
var form = document.getElementById('form');
var btn = document.getElementById('btn');
var selVersion = document.getElementById('sel-version');
var selDeploy = document.getElementById('sel-deploy');
var selLicense = document.getElementById('sel-license');
var msgs = document.getElementById('messages');
var chatHistory = [];

q.addEventListener('input', function() {
  this.style.height = 'auto';
  this.style.height = Math.min(this.scrollHeight, 160) + 'px';
});

q.addEventListener('keydown', function(e) {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); form.dispatchEvent(new Event('submit')); }
});

function fill(s) { q.value = s; q.focus(); }

function escHtml(s) { return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function filename(p) { return p.split('/').pop(); }

var _tick = String.fromCharCode(96);
var _codeRe = new RegExp('(' + _tick + '[^' + _tick + '\\n]+' + _tick + ')');

function inlineMd(t) {
  var parts = t.split(_codeRe);
  return parts.map(function(s, i) {
    if (i % 2 === 1) return '<code>' + escHtml(s.slice(1, -1)) + '</code>';
    s = s.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/\*(.+?)\*/g, '<em>$1</em>');
    return s;
  }).join('');
}

function isFence(l) { return l.length >= 3 && l.charCodeAt(0) === 96 && l.charCodeAt(1) === 96 && l.charCodeAt(2) === 96; }

function parseTable(tableLines) {
  if (!tableLines.length) return '';
  function cells(line) { return line.replace(/^\||\|$/g,'').split('|').map(function(c){return c.trim();}); }
  function isSep(line) { return /^\|[\s\-:|]+\|[\s\-:|]*$/.test(line.trim()); }
  var hasSep = tableLines.length > 1 && isSep(tableLines[1]);
  var h = '<div class="tbl-wrap"><table>';
  if (hasSep) {
    h += '<thead><tr>';
    cells(tableLines[0]).forEach(function(c){h+='<th>'+inlineMd(c)+'</th>';});
    h += '</tr></thead><tbody>';
    for (var j=2;j<tableLines.length;j++){h+='<tr>';cells(tableLines[j]).forEach(function(c){h+='<td>'+inlineMd(c)+'</td>';});h+='</tr>';}
    h += '</tbody>';
  } else {
    h += '<tbody>';
    tableLines.forEach(function(line){h+='<tr>';cells(line).forEach(function(c){h+='<td>'+inlineMd(c)+'</td>';});h+='</tr>';});
    h += '</tbody>';
  }
  return h + '</table></div>';
}

function renderMd(raw) {
  var lines = raw.split('\n'), out = '', i = 0;
  while (i < lines.length) {
    var l = lines[i];
    if (isFence(l)) {
      var code = ''; i++;
      while (i < lines.length && !isFence(lines[i])) { code += lines[i] + '\n'; i++; }
      out += '<pre><code>' + escHtml(code.trimEnd()) + '</code></pre>';
      i++; continue;
    }
    if (l.startsWith('#### ')) { out += '<h5>' + inlineMd(l.slice(5)) + '</h5>'; i++; continue; }
    if (l.startsWith('### '))  { out += '<h4>' + inlineMd(l.slice(4)) + '</h4>'; i++; continue; }
    if (l.startsWith('## '))   { out += '<h3>' + inlineMd(l.slice(3)) + '</h3>'; i++; continue; }
    if (l.startsWith('# '))    { out += '<h2>' + inlineMd(l.slice(2)) + '</h2>'; i++; continue; }
    if (/^[-*] /.test(l)) {
      out += '<ul>';
      while (i < lines.length && /^[-*] /.test(lines[i])) { out += '<li>' + inlineMd(lines[i].slice(2)) + '</li>'; i++; }
      out += '</ul>'; continue;
    }
    if (/^\d+\. /.test(l)) {
      out += '<ol>';
      while (i < lines.length && /^\d+\. /.test(lines[i])) { out += '<li>' + inlineMd(lines[i].replace(/^\d+\. /,'')) + '</li>'; i++; }
      out += '</ol>'; continue;
    }
    if (l.trimStart().startsWith('|')) {
      var tbl = [];
      while (i < lines.length && lines[i].trimStart().startsWith('|')) { tbl.push(lines[i]); i++; }
      out += parseTable(tbl); continue;
    }
    if (l.trim() === '') { out += '<br>'; i++; continue; }
    out += '<p>' + inlineMd(l) + '</p>';
    i++;
  }
  return out;
}

var twWords = ['Thinking','Searching','Retrieving','Analyzing','Reading','Reasoning','Processing'];
var twTimer = null, twOn = false;

function startThinking(bubble) {
  bubble.innerHTML = '<span class="tw"></span><span class="cursor"></span>';
  twOn = true;
  twType(bubble.querySelector('.tw'), 0);
}

function stopThinking() { twOn = false; if (twTimer) { clearTimeout(twTimer); twTimer = null; } }

function twType(el, idx) {
  if (!twOn || !el) return;
  var word = twWords[idx % twWords.length] + '...';
  var i = 0;
  function tick() {
    if (!twOn) return;
    el.textContent = word.slice(0, i);
    if (i <= word.length) { i++; twTimer = setTimeout(tick, 42); }
    else {
      var wait = 5000 + Math.floor(Math.random() * 5001);
      twTimer = setTimeout(function() { twErase(el, word, idx); }, wait);
    }
  }
  tick();
}

function twErase(el, word, idx) {
  if (!twOn) return;
  var i = word.length;
  function tick() {
    if (!twOn) return;
    el.textContent = word.slice(0, i);
    if (i > 0) { i--; twTimer = setTimeout(tick, 32); }
    else { twTimer = setTimeout(function() { twType(el, idx + 1); }, 180); }
  }
  tick();
}

form.addEventListener('submit', async function(e) {
  e.preventDefault();
  var question = q.value.trim();
  if (!question) return;

  var empty = document.getElementById('empty');
  if (empty) empty.remove();

  q.value = ''; q.style.height = 'auto'; btn.disabled = true; btn.innerHTML = '<span class="ldot"></span><span class="ldot"></span><span class="ldot"></span>';

  var qDiv = document.createElement('div');
  qDiv.className = 'message';
  qDiv.innerHTML =
    '<div class="role r">_you/</div>' +
    '<div class="row r"><div class="bubble u">' + escHtml(question) + '</div></div>';
  msgs.appendChild(qDiv);

  var aDiv = document.createElement('div');
  aDiv.className = 'message';
  aDiv.innerHTML =
    '<div class="role">_formance/</div>' +
    '<div class="row"><div class="bubble a" id="cur"><span class="cursor"></span></div></div>';
  msgs.appendChild(aDiv);
  msgs.scrollTop = msgs.scrollHeight;

  var bubble = document.getElementById('cur');
  bubble.removeAttribute('id');
  var text = '';

  startThinking(bubble);

  try {
    var res = await fetch('/api/ask', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({question: question, top_k: 15, history: chatHistory, version: selVersion.value, deployment: selDeploy.value, license: selLicense.value})
    });
    if (res.status === 429) {
      stopThinking();
      bubble.innerHTML = '<span style="color:#e74c3c">Trop de requêtes — réessaie dans une minute.</span>';
      btn.disabled = false; btn.textContent = 'Send'; q.focus();
      return;
    }
    var reader = res.body.getReader();
    var dec = new TextDecoder();
    var buf = '';

    while (true) {
      var r = await reader.read();
      if (r.done) break;
      buf += dec.decode(r.value, {stream: true});
      var lines = buf.split('\n');
      buf = lines.pop();

      for (var i = 0; i < lines.length; i++) {
        var line = lines[i].trim();
        if (!line.startsWith('data: ')) continue;
        var ev;
        try { ev = JSON.parse(line.slice(6)); } catch(_) { continue; }

        if (ev.type === 'chunk') {
          text += ev.text || '';
          stopThinking();
          bubble.innerHTML = renderMd(text) + '<span class="cursor"></span>';
          msgs.scrollTop = msgs.scrollHeight;

        } else if (ev.type === 'sources' && ev.sources) {
          var srcDiv = document.createElement('div');
          srcDiv.className = 'sources';
          var seen = {};
          ev.sources.forEach(function(s) {
            if (seen[s.file]) return;
            seen[s.file] = true;
            var chip = document.createElement('div');
            chip.className = 'chip';
            chip.title = s.section ? s.file + ' › ' + s.section : s.file;
            chip.textContent = '_ ' + filename(s.file);
            srcDiv.appendChild(chip);
          });
          aDiv.appendChild(srcDiv);

        } else if (ev.type === 'done') {
          stopThinking();
          bubble.innerHTML = renderMd(text);
          // Feedback buttons
          var fbRow = document.createElement('div');
          fbRow.className = 'fb-row';
          fbRow.innerHTML = '<span class="fb-label">Cette réponse t\'a aidé ?</span>'
            + '<button class="fb-btn" data-r="1">👍</button>'
            + '<button class="fb-btn" data-r="-1">👎</button>';
          fbRow.querySelectorAll('.fb-btn').forEach(function(fb) {
            fb.addEventListener('click', function() {
              fetch('/api/feedback', {method:'POST', headers:{'Content-Type':'application/json'},
                body: JSON.stringify({question: question, answer: text, rating: parseInt(fb.getAttribute('data-r'))})});
              fbRow.innerHTML = '<span class="fb-thanks">'
                + (parseInt(fb.getAttribute('data-r')) === 1 ? 'Merci 😊' : 'Merci, on améliore 📌')
                + '</span>';
            });
          });
          aDiv.appendChild(fbRow);

        } else if (ev.type === 'error') {
          stopThinking();
          bubble.innerHTML = '<span style="color:#c0392b">Error: ' + escHtml(ev.text) + '</span>';
        }
      }
    }
  } catch(err) {
    stopThinking();
    bubble.innerHTML = '<span style="color:#c0392b">Network error: ' + escHtml(err.message) + '</span>';
  }

  chatHistory.push({role: 'user', content: question});
  chatHistory.push({role: 'assistant', content: text});

  btn.disabled = false; btn.textContent = 'Send'; q.focus();
});
</script>
</body>
</html>`
