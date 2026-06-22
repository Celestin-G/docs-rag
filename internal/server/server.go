package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/store"
)

// New wires up HTTP handlers and returns a ready handler.
func New(st *store.Store, emb *embedder.Embedder, lm *llm.Client) http.Handler {
	s := &srv{store: st, emb: emb, llm: lm}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	return mux
}

type srv struct {
	store *store.Store
	emb   *embedder.Embedder
	llm   *llm.Client
}

func (s *srv) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, ui)
}

// --- /api/ask -----------------------------------------------------------------

type askReq struct {
	Question string        `json:"question"`
	TopK     int           `json:"top_k"`
	History  []llm.Message `json:"history"`
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

func (s *srv) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.TopK <= 0 {
		req.TopK = 15
	}

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

	// Translate to English before embedding so the query matches the English docs.
	queryEn, err := s.llm.Translate(ctx, req.Question)
	if err != nil {
		queryEn = req.Question // graceful fallback
	}

	vec, err := s.emb.Embed(ctx, queryEn)
	if err != nil {
		send(sseEvent{Type: "error", Text: "Embedding failed: " + err.Error()})
		return
	}

	// Fetch more candidates than needed, then apply a diversity filter so no
	// single file monopolises the context window (e.g. a long file whose
	// "writes" section ranks 1st doesn't crowd out its own "reads" section).
	rawChunks, err := s.store.Search(ctx, vec, req.TopK*2, 0.38)
	if err != nil {
		send(sseEvent{Type: "error", Text: "Search failed: " + err.Error()})
		return
	}
	seenFile := map[string]int{}
	var chunks []store.DocChunk
	for _, c := range rawChunks {
		if seenFile[c.SourceFile] >= 2 {
			continue
		}
		seenFile[c.SourceFile]++
		chunks = append(chunks, c)
		if len(chunks) >= req.TopK {
			break
		}
	}
	for _, c := range chunks {
		log.Printf("retrieval score=%.3f  section=%-30s  file=%s", c.Score, c.Section, c.SourceFile)
	}
	if len(chunks) == 0 {
		send(sseEvent{Type: "chunk", Text: "No relevant passage found in the documentation."})
		send(sseEvent{Type: "done"})
		return
	}

	ctxChunks := make([]llm.ContextChunk, len(chunks))
	for i, c := range chunks {
		ctxChunks[i] = llm.ContextChunk{SourceFile: c.SourceFile, Section: c.Section, Content: c.Content}
	}

	if err := s.llm.Ask(ctx, queryEn, req.History, ctxChunks, &sseWriter{w: w, flusher: flusher}); err != nil {
		send(sseEvent{Type: "error", Text: "LLM error: " + err.Error()})
		return
	}

	sources := make([]source, len(chunks))
	for i, c := range chunks {
		sources[i] = source{File: c.SourceFile, Section: c.Section}
	}
	send(sseEvent{Type: "sources", Sources: sources})
	send(sseEvent{Type: "done"})
}

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

/* Input bar */
#form-wrap{background:var(--slate-10);border-top:1px solid var(--emerald-20);padding:16px 24px;flex-shrink:0}
#form{max-width:760px;margin:0 auto;display:flex;align-items:stretch}
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
    <div class="e-eyebrow">_rag/</div>
    <h2 class="e-heading">Ask a question about the Formance platform.</h2>
    <p class="e-sub">Vector search across the official documentation.<br>Answers generated locally by Ollama.</p>
    <div class="suggestions">
      <div class="sug" onclick="fill('What is Numscript?')">What is Numscript?</div>
      <div class="sug" onclick="fill('How does the Ledger module work?')">How does the Ledger work?</div>
      <div class="sug" onclick="fill('How do I configure a payment connector?')">Configure a connector</div>
      <div class="sug" onclick="fill('What are Wallets used for?')">What are Wallets?</div>
    </div>
  </div>
</div>

<div id="form-wrap">
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
      body: JSON.stringify({question: question, top_k: 15, history: chatHistory})
    });
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
