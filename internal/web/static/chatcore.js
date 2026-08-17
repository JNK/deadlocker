/* Shared chat machinery for the builder sheet and the discuss bubble.

   The conversation is rendered as a sequence of blocks rather than one bubble
   per turn: prose, reasoning and tool calls all interrupt each other, and
   showing them inline in the order they happened is the only way the reply
   makes sense. A new prose block starts whenever a tool call or a reasoning
   block lands, so text either side of a tool call does not merge into one
   confusing paragraph. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var md = function (t) { return window.DL.renderMarkdown(t); };

  function fmtDuration(ms) {
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
  }

  // ------------------------------------------------------------------- log

  function Log(logEl, emptyEl) {
    this.el = logEl;
    this.empty = emptyEl;
    this.textBlock = null;     // open prose block, if any
    this.reasoning = {};       // id -> open reasoning block
    this.tools = {};           // id -> open tool block
    this.pinned = true;        // follow the tail unless the user scrolls up
    var self = this;
    logEl.addEventListener('scroll', function () {
      var slack = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight;
      self.pinned = slack < 60;
    });
  }

  Log.prototype.scroll = function () {
    if (this.pinned) this.el.scrollTop = this.el.scrollHeight;
  };

  Log.prototype.add = function (node) {
    if (this.empty) this.empty.hidden = true;
    this.el.appendChild(node);
    this.scroll();
    return node;
  };

  Log.prototype.clear = function () {
    this.el.innerHTML = '';
    this.textBlock = null;
    this.reasoning = {};
    this.tools = {};
    this.pinned = true;
    if (this.empty) {
      this.el.appendChild(this.empty);
      this.empty.hidden = false;
    }
  };

  // A tool call or reasoning block ends the current prose block, so the next
  // text starts its own bubble.
  Log.prototype.breakText = function () { this.textBlock = null; };

  Log.prototype.user = function (text) {
    var div = document.createElement('div');
    div.className = 'chat-msg chat-user';
    var body = document.createElement('div');
    body.className = 'chat-body';
    body.textContent = text;
    div.appendChild(body);
    this.breakText();
    return this.add(div);
  };

  // queued renders a message the user sent while a reply was still streaming.
  Log.prototype.queued = function (text) {
    var div = document.createElement('div');
    div.className = 'chat-msg chat-user is-queued';
    var body = document.createElement('div');
    body.className = 'chat-body';
    body.textContent = text;
    var tag = document.createElement('span');
    tag.className = 'chat-queued-tag';
    tag.textContent = 'queued';
    div.appendChild(body);
    div.appendChild(tag);
    this.add(div);
    return div;
  };

  Log.prototype.appendText = function (text) {
    if (!this.textBlock) {
      var div = document.createElement('div');
      div.className = 'chat-msg chat-assistant';
      var body = document.createElement('div');
      body.className = 'chat-body chat-md';
      div.appendChild(body);
      this.add(div);
      this.textBlock = { el: body, raw: '', frame: 0 };
    }
    var block = this.textBlock;
    block.raw += text;
    // Re-rendering markdown on every token is wasteful; coalesce to a frame.
    if (!block.frame) {
      var self = this;
      block.frame = requestAnimationFrame(function () {
        block.frame = 0;
        block.el.innerHTML = md(block.raw);
        self.scroll();
      });
    }
  };

  // ------------------------------------------------------------- reasoning

  Log.prototype.reasoningStart = function (id) {
    this.breakText();
    var div = document.createElement('div');
    div.className = 'chat-think is-streaming';
    div.innerHTML =
      '<button class="think-head" type="button">' +
      '<span class="think-spinner"></span>' +
      '<span class="think-label">Thinking…</span>' +
      '<span class="think-caret">▾</span>' +
      '</button>' +
      '<div class="think-body"><div class="think-text"></div></div>';

    var block = {
      el: div,
      text: div.querySelector('.think-text'),
      body: div.querySelector('.think-body'),
      label: div.querySelector('.think-label'),
      raw: '',
      started: Date.now(),
      open: true
    };

    div.querySelector('.think-head').addEventListener('click', function () {
      block.open = !block.open;
      div.classList.toggle('is-open', block.open);
      div.classList.toggle('is-collapsed', !block.open);
    });

    this.reasoning[id || 'default'] = block;
    return this.add(div);
  };

  Log.prototype.reasoningDelta = function (id, text) {
    var block = this.reasoning[id || 'default'];
    if (!block) {
      this.reasoningStart(id);
      block = this.reasoning[id || 'default'];
    }
    block.raw += text;
    block.text.textContent = block.raw;
    // While streaming the body is a fixed-height window showing the tail, so a
    // long chain of thought does not push the conversation off screen.
    block.body.scrollTop = block.body.scrollHeight;
    this.scroll();
  };

  Log.prototype.reasoningEnd = function (id) {
    var key = id || 'default';
    var block = this.reasoning[key];
    if (!block) return;
    var secs = Math.max(1, Math.round((Date.now() - block.started) / 1000));
    block.el.classList.remove('is-streaming');
    block.el.classList.add('is-collapsed');
    block.open = false;
    block.label.textContent = 'Thought for ' + secs + 's';
    delete this.reasoning[key];
    this.breakText();
    this.scroll();
  };

  // ----------------------------------------------------------------- tools

  Log.prototype.toolCall = function (ev) {
    this.breakText();
    var div = document.createElement('div');
    div.className = 'chat-tool is-running';

    var label = ev.label || ev.tool;
    div.innerHTML =
      '<div class="tool-head">' +
      '<span class="tool-spinner"></span>' +
      '<span class="tool-label"></span>' +
      '<span class="tool-detail"></span>' +
      '<span class="tool-time"></span>' +
      '<button class="tool-toggle" type="button" title="Show the arguments and result">⋯</button>' +
      '</div>' +
      '<div class="tool-payload" hidden>' +
      '<div class="tool-section"><span class="tool-section-title">arguments</span><pre></pre></div>' +
      '<div class="tool-section tool-result-section" hidden><span class="tool-section-title">result</span><pre></pre></div>' +
      '</div>';

    div.querySelector('.tool-label').textContent = label;
    div.querySelector('.tool-detail').textContent = ev.detail || '';
    div.querySelector('.tool-payload .tool-section pre').textContent = prettyJSON(ev.input);

    var payload = div.querySelector('.tool-payload');
    div.querySelector('.tool-toggle').addEventListener('click', function () {
      payload.hidden = !payload.hidden;
      div.classList.toggle('is-expanded', !payload.hidden);
    });

    var block = { el: div, started: Date.now(), timer: 0 };
    var timeEl = div.querySelector('.tool-time');
    block.timer = setInterval(function () {
      timeEl.textContent = fmtDuration(Date.now() - block.started);
    }, 250);

    this.tools[ev.id || ev.tool] = block;
    return this.add(div);
  };

  Log.prototype.toolResult = function (ev) {
    var key = ev.id || ev.tool;
    var block = this.tools[key];
    if (!block) return;
    clearInterval(block.timer);

    var div = block.el;
    div.classList.remove('is-running');
    div.classList.add(ev.failed ? 'is-failed' : 'is-done');
    div.querySelector('.tool-time').textContent = fmtDuration(Date.now() - block.started);

    var detail = div.querySelector('.tool-detail');
    if (ev.summary) {
      detail.textContent = ev.summary;
      detail.classList.add('is-outcome');
    }
    if (ev.result) {
      var section = div.querySelector('.tool-result-section');
      section.hidden = false;
      section.querySelector('pre').textContent = prettyJSON(ev.result);
    }
    delete this.tools[key];
    this.breakText();
    this.scroll();
  };

  Log.prototype.note = function (kind, text) {
    this.breakText();
    var div = document.createElement('div');
    div.className = 'chat-note chat-note-' + kind;
    div.textContent = text;
    return this.add(div);
  };

  // ------------------------------------------------------ processing bubble

  // A placeholder assistant bubble so the wait reads as "the assistant is
  // working" in the conversation itself, not just as small print at the bottom.
  Log.prototype.startPending = function () {
    var div = document.createElement('div');
    div.className = 'chat-msg chat-assistant chat-pending';
    div.innerHTML =
      '<div class="chat-body">' +
      '<span class="pending-dots"><i></i><i></i><i></i></span>' +
      '<span class="pending-label">Processing</span>' +
      '<span class="pending-time"></span>' +
      '</div>';
    this.add(div);

    var started = Date.now();
    var timeEl = div.querySelector('.pending-time');
    var timer = setInterval(function () {
      timeEl.textContent = fmtDuration(Date.now() - started);
    }, 250);

    var self = this;
    return {
      // Called on the first sign of life; the bubble has done its job.
      done: function () {
        clearInterval(timer);
        div.remove();
        self.scroll();
      },
      elapsed: function () { return Date.now() - started; }
    };
  };

  function prettyJSON(raw) {
    if (!raw) return '';
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch (e) {
      return raw;
    }
  }

  // ------------------------------------------------------------- streaming

  function readStream(stream, onEvent) {
    var reader = stream.getReader();
    var decoder = new TextDecoder();
    var buffer = '';

    function pump() {
      return reader.read().then(function (res) {
        if (res.done) return;
        buffer += decoder.decode(res.value, { stream: true });
        var chunks = buffer.split('\n\n');
        buffer = chunks.pop();
        chunks.forEach(function (chunk) {
          var event = 'message';
          var data = [];
          chunk.split('\n').forEach(function (line) {
            if (line.indexOf('event:') === 0) event = line.slice(6).trim();
            else if (line.indexOf('data:') === 0) data.push(line.slice(5).trim());
          });
          if (!data.length) return;
          var parsed;
          try { parsed = JSON.parse(data.join('\n')); } catch (e) { return; }
          onEvent(event, parsed);
        });
        return pump();
      });
    }
    return pump();
  }

  // runTurn streams one exchange into a Log, forwarding anything the caller
  // cares about (draft updates, run ids) through handlers.
  function runTurn(session, message, log, handlers) {
    var controller = new AbortController();
    var pending = log.startPending();
    var settled = false;
    function firstSignal() {
      if (settled) return;
      settled = true;
      pending.done();
    }

    var promise = fetch('/api/chat/' + session + '/send', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: message }),
      signal: controller.signal
    }).then(function (resp) {
      if (!resp.ok || !resp.body) {
        throw new Error('the assistant endpoint returned ' + resp.status);
      }
      return readStream(resp.body, function (event, data) {
        switch (event) {
          case 'delta':
            firstSignal();
            log.appendText(data.text || '');
            break;
          case 'reasoning_start':
            firstSignal();
            log.reasoningStart(data.id);
            break;
          case 'reasoning_delta':
            firstSignal();
            log.reasoningDelta(data.id, data.text || '');
            break;
          case 'reasoning_end':
            log.reasoningEnd(data.id);
            break;
          case 'tool_call':
            firstSignal();
            log.toolCall(data);
            break;
          case 'tool_result':
            log.toolResult(data);
            break;
          case 'done':
            firstSignal();
            log.breakText();
            break;
          case 'error':
            firstSignal();
            log.note('error', data.message || 'something went wrong');
            break;
          default:
            firstSignal();
            if (handlers[event]) handlers[event](data);
        }
      });
    }).finally(function () {
      firstSignal();
    });

    return { promise: promise, abort: function () { controller.abort(); } };
  }

  function createSession(mode, scenarioID, runID) {
    return window.DL.postJSON('/api/chat', {
      mode: mode, scenario_id: scenarioID || '', run_id: runID || ''
    });
  }

  function chatStatus() {
    return fetch('/api/chat/status').then(function (r) { return r.json(); });
  }

  function wireSuggestions(host, items, onPick) {
    if (!host) return;
    host.innerHTML = items.map(function (s) {
      return '<button class="chat-suggestion" type="button">' + esc(s) + '</button>';
    }).join('');
    Array.prototype.forEach.call(host.children, function (b) {
      b.addEventListener('click', function () { onPick(b.textContent); });
    });
  }

  // loadSuggestions asks the server for a fresh random selection, so opening
  // the builder twice offers different starting points.
  function loadSuggestions(host, mode, onPick) {
    if (!host) return;
    fetch('/api/chat/prompts?mode=' + encodeURIComponent(mode) + '&n=3')
      .then(function (r) { return r.json(); })
      .then(function (res) {
        if (res && res.prompts && res.prompts.length) {
          wireSuggestions(host, res.prompts, onPick);
        }
      })
      .catch(function () { /* suggestions are a nicety, not a requirement */ });
  }

  // Composer supports queueing: a message typed while a reply is streaming is
  // held and sent as soon as the turn finishes, rather than being refused.
  function Composer(textarea, button, statusEl, log, opts) {
    this.textarea = textarea;
    this.button = button;
    this.status = statusEl;
    this.log = log;
    this.opts = opts;
    this.queue = [];
    this.busy = false;
    this.idleText = opts.idleText || 'Enter to send · Shift+Enter for a newline';

    var self = this;
    textarea.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        self.submit();
      }
    });
    textarea.addEventListener('input', function () { self.grow(); });
    button.addEventListener('click', function () { self.submit(); });
    this.grow();
    this.paint();
  }

  // grow sizes the textarea to its content. The CSS removes the drag handle, so
  // this is the only thing that changes its height; max-height in CSS caps it
  // and hands back a scrollbar.
  Composer.prototype.grow = function () {
    var ta = this.textarea;
    ta.style.height = 'auto';
    var max = parseFloat(getComputedStyle(ta).maxHeight);
    var next = ta.scrollHeight;
    if (!isNaN(max) && next > max) {
      ta.style.height = max + 'px';
      ta.style.overflowY = 'auto';
    } else {
      ta.style.height = next + 'px';
      ta.style.overflowY = 'hidden';
    }
  };

  Composer.prototype.paint = function () {
    if (this.busy) {
      this.button.textContent = this.queue.length ? 'Queue (' + this.queue.length + ')' : 'Queue';
      this.status.textContent = this.queue.length
        ? this.queue.length + ' message(s) queued — they will send when this reply finishes'
        : 'Replying… you can type the next message now';
    } else {
      this.button.textContent = 'Send';
      this.status.textContent = this.idleText;
    }
  };

  Composer.prototype.submit = function () {
    var text = this.textarea.value.trim();
    if (!text || !this.opts.ready()) return;
    this.textarea.value = '';
    this.grow();

    if (this.busy) {
      var node = this.log.queued(text);
      this.queue.push({ text: text, node: node });
      this.paint();
      return;
    }
    this.send(text);
  };

  Composer.prototype.send = function (text) {
    this.busy = true;
    this.paint();
    this.log.user(text);
    var self = this;
    this.opts.onSend(text).finally(function () {
      self.busy = false;
      var next = self.queue.shift();
      if (next) {
        // Promote the queued bubble to a normal one and send it.
        next.node.remove();
        self.paint();
        self.send(next.text);
        return;
      }
      self.paint();
    });
  };

  Composer.prototype.isBusy = function () { return this.busy || this.queue.length > 0; };

  window.DL = window.DL || {};
  window.DL.chat = {
    Log: Log,
    Composer: Composer,
    runTurn: runTurn,
    createSession: createSession,
    status: chatStatus,
    wireSuggestions: wireSuggestions,
    loadSuggestions: loadSuggestions,
    fmtDuration: fmtDuration
  };
})();
