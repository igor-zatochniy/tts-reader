package main

const localDashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Audiobook TTS Reader</title>
  <style>
    :root { color-scheme: light dark; font-family: Segoe UI, system-ui, sans-serif; }
    body { margin: 0; background: #f6f7f9; color: #18202a; }
    main { max-width: 960px; margin: 0 auto; padding: 28px; }
    h1 { margin: 0 0 20px; font-size: 28px; }
    section { background: #fff; border: 1px solid #d8dee8; border-radius: 8px; padding: 18px; margin-bottom: 16px; }
    label { display: block; font-size: 13px; font-weight: 600; margin-bottom: 6px; color: #3b4654; }
    input, select { width: 100%; box-sizing: border-box; padding: 10px 12px; border: 1px solid #b9c3d1; border-radius: 6px; font: inherit; }
    button { padding: 10px 14px; border: 0; border-radius: 6px; background: #1d6fd8; color: white; font: inherit; cursor: pointer; }
    button.secondary { background: #465568; }
    button.danger { background: #b42318; }
    button:disabled { opacity: .55; cursor: not-allowed; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .actions { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 14px; }
    .state { display: grid; grid-template-columns: repeat(4, 1fr); gap: 10px; }
    .metric { border: 1px solid #d8dee8; border-radius: 8px; padding: 12px; background: #fbfcfe; }
    .metric strong { display: block; font-size: 12px; color: #596779; margin-bottom: 4px; }
    pre { min-height: 140px; overflow: auto; padding: 12px; background: #101820; color: #d8f3dc; border-radius: 8px; }
    @media (max-width: 760px) { .grid, .state { grid-template-columns: 1fr; } main { padding: 16px; } }
  </style>
</head>
<body>
<main>
  <h1>Audiobook TTS Reader</h1>

  <section>
    <div class="grid">
      <div>
        <label for="bookPath">Book path</label>
        <input id="bookPath" placeholder="C:\Books\novel.txt">
      </div>
      <div>
        <label for="title">Title</label>
        <input id="title" placeholder="Optional">
      </div>
      <div>
        <label for="voice">Voice</label>
        <select id="voice"></select>
      </div>
      <div>
        <label for="chunkSize">Chunk size</label>
        <input id="chunkSize" type="number" min="1" value="400">
      </div>
    </div>
    <div class="actions">
      <button id="addBook">Add book</button>
      <button id="play">Play</button>
      <button class="secondary" id="pause">Pause</button>
      <button class="secondary" id="resume">Resume</button>
      <button class="danger" id="stop">Stop</button>
    </div>
  </section>

  <section>
    <div class="state">
      <div class="metric"><strong>State</strong><span id="state">stopped</span></div>
      <div class="metric"><strong>Progress</strong><span id="progress">0%</span></div>
      <div class="metric"><strong>Current byte</strong><span id="currentByte">0</span></div>
      <div class="metric"><strong>Book</strong><span id="bookId">none</span></div>
    </div>
    <div class="actions">
      <input id="position" type="number" min="0" placeholder="Byte position">
      <button class="secondary" id="setPosition">Set position</button>
    </div>
  </section>

  <section>
    <pre id="events"></pre>
  </section>
</main>
<script>
let currentBookId = "";
const $ = (id) => document.getElementById(id);
const apiToken = new URLSearchParams(location.search).get("token") || "";

async function api(path, options = {}) {
  const headers = { "Content-Type": "application/json", ...(options.headers || {}) };
  if (apiToken) headers["X-TTS-Token"] = apiToken;
  const response = await fetch(path, {
    ...options,
    headers
  });
  const text = await response.text();
  const data = text ? JSON.parse(text) : {};
  if (!response.ok) throw new Error(data.error || response.statusText);
  return data;
}

function render(snapshot) {
  $("state").textContent = snapshot.state || "unknown";
  $("progress").textContent = Number(snapshot.progress_percent || 0).toFixed(2) + "%";
  $("currentByte").textContent = snapshot.current_byte || 0;
  $("bookId").textContent = snapshot.book_id || currentBookId || "none";
}

function log(line) {
  const box = $("events");
  box.textContent = new Date().toLocaleTimeString() + "  " + line + "\n" + box.textContent;
}

async function refreshState() {
  render(await api("/api/v1/playback"));
}

async function loadVoices() {
  const data = await api("/api/v1/voices");
  $("voice").innerHTML = '<option value="">System default</option>' + data.voices.map((item) => {
    const voice = item.name || item;
    return '<option value="' + voice.replaceAll('"', "&quot;") + '">' + voice + "</option>";
  }
  ).join("");
}

$("addBook").onclick = async () => {
  const book = await api("/api/v1/books", {
    method: "POST",
    body: JSON.stringify({ path: $("bookPath").value, title: $("title").value })
  });
  currentBookId = book.id;
  log("book.added " + book.id);
  await refreshState();
};

$("play").onclick = async () => {
  if (!currentBookId) throw new Error("Add a book first");
  render(await api("/api/v1/playback", {
    method: "POST",
    body: JSON.stringify({
      book_id: currentBookId,
      voice: $("voice").value,
      chunk_size: Number($("chunkSize").value || 400)
    })
  }));
};

$("pause").onclick = async () => render(await api("/api/v1/playback/pause", { method: "POST" }));
$("resume").onclick = async () => render(await api("/api/v1/playback/resume", { method: "POST" }));
$("stop").onclick = async () => render(await api("/api/v1/playback/stop", { method: "POST" }));
$("setPosition").onclick = async () => {
  if (!currentBookId) throw new Error("Add a book first");
  render(await api("/api/v1/playback/position", {
    method: "PUT",
    body: JSON.stringify({ book_id: currentBookId, current_byte: Number($("position").value || 0) })
  }));
};

const source = new EventSource("/api/v1/events" + (apiToken ? "?token=" + encodeURIComponent(apiToken) : ""));
["playback.started", "chunk.started", "progress.updated", "playback.paused", "playback.resumed", "playback.stopped", "playback.finished", "playback.failed", "position.updated"].forEach((name) => {
  source.addEventListener(name, (event) => {
    const data = JSON.parse(event.data);
    render(data.playback);
    log(name + " " + JSON.stringify(data.playback));
  });
});

loadVoices().then(refreshState).catch((err) => log("error " + err.message));
window.addEventListener("unhandledrejection", (event) => log("error " + event.reason.message));
</script>
</body>
</html>`
