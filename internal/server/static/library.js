(function () {
  "use strict";
  var token = null;
  try { token = sessionStorage.getItem("tinysonic.token"); } catch (e) {}
  if (!token) { location.replace("login"); return; }

  function $(id) { return document.getElementById(id); }
  function say(t) { $("notice").textContent = t; }
  function num(n) { return Number(n).toLocaleString(); }
  function hours(sec) {
    var h = Math.floor(sec / 3600), m = Math.round((sec % 3600) / 60);
    return h + " h " + m + " min";
  }
  function when(s) {
    if (!s) return "never";
    var d = new Date(s);
    return isNaN(d) ? s : d.toLocaleString();
  }
  async function api(method, path, body) {
    var opts = { method: method, headers: { "Authorization": "Bearer " + token } };
    if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
    var r = await fetch("api/" + path, opts);
    if (r.status === 401) {
      try { sessionStorage.removeItem("tinysonic.token"); } catch (e) {}
      location.replace("login");
      throw new Error("signed out");
    }
    var j = await r.json().catch(function () { return {}; });
    if (r.status === 403) { location.replace("account"); throw new Error("librarians only"); }
    if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
    return j;
  }

  var timer = null;
  function render(st) {
    $("songs").textContent = num(st.songs);
    $("albums").textContent = num(st.albums);
    $("artists").textContent = num(st.artists);
    $("duration").textContent = hours(st.duration);
    $("lastScan").textContent = when(st.lastScan);

    var b = st.bpm, total = Math.max(1, st.songs);
    $("bpmTag").textContent = num(b.tag);
    $("bpmCalc").textContent = num(b.calc);
    $("bpmMissing").textContent = num(b.missing);
    $("barTag").style.width = (100 * b.tag / total) + "%";
    $("barCalc").style.width = (100 * b.calc / total) + "%";

    var scanning = st.scan && st.scan.running;
    var job = st.job || {}, jobRunning = !!job.running;
    var busy = scanning || jobRunning;
    $("scanState").hidden = !scanning;
    if (scanning) $("scanState").textContent = "Scanning… " + num(st.scan.count) + " files read";

    var ff = st.ffmpeg || {};
    $("ffmpegWarn").hidden = !!ff.available;
    if (!ff.available) $("ffmpegWarn").textContent = "BPM calculation is unavailable: " + (ff.reason || "ffmpeg not found") + ".";
    $("scan").disabled = busy;
    $("bpmMissingBtn").disabled = busy || !ff.available || b.missing === 0;
    $("bpmAllBtn").disabled = busy || !ff.available;

    $("job").hidden = !jobRunning;
    if (jobRunning) {
      $("jobBar").max = Math.max(1, job.total);
      $("jobBar").value = job.done;
      $("jobText").textContent = num(job.done) + " of " + num(job.total) + " tracks analysed, " +
        num(job.updated) + " stored, " + num(job.failed) + " failed";
    }
    $("jobDetail").hidden = !jobRunning;
    if (jobRunning) {
      var detail = num(job.pending) + " pending, " + job.songsPerMinute + " songs/min";
      if (job.current) detail += ", current " + job.current;
      if (job.lastError) detail += ". Last error: " + job.lastError;
      $("jobDetail").textContent = detail;
    }
    $("jobLast").hidden = jobRunning || !job.kind;
    if (!jobRunning && job.kind) {
      var label = {all: "recalculate", missing: "missing", recalc: "recalculate analysis", pending: "analysis after scan"}[job.mode] || job.mode;
      $("jobLast").textContent = "Last run (" + label + "), finished " +
        when(job.finishedAt) + ": " + num(job.updated) + " stored, " + num(job.failed) + " failed" +
        (job.error ? " — " + job.error : "") + ".";
    }

    var failedSongs = st.failedSongs || 0;
    $("failed").hidden = failedSongs === 0;
    if (failedSongs !== shownFailed) {
      shownFailed = failedSongs;
      loadFailed();
    }

    schedule(busy ? 5000 : 30000);
  }

  // Only one poll is pending, and none while the tab is hidden: a hidden page
  // polls nothing, and showing it again refreshes at once (see below).
  function schedule(ms) {
    clearTimeout(timer);
    timer = null;
    if (!document.hidden) timer = setTimeout(refresh, ms);
  }

  // The failed list is loaded when its count changes, not on every poll.
  var shownFailed = -1, listed = [];
  async function loadFailed() {
    try {
      var res = await api("GET", "library/analysis/failed?limit=200");
      listed = res.songs || [];
      $("failedCount").textContent = num(res.total) + " track" + (res.total === 1 ? "" : "s") + " failed.";
      var body = $("failedBody");
      while (body.firstChild) body.removeChild(body.firstChild);
      listed.forEach(function (row) {
        var tr = document.createElement("tr");
        var track = document.createElement("td");
        track.textContent = row.artist + " — " + row.title;
        var tool = document.createElement("td");
        tool.textContent = row.tool;
        var msg = document.createElement("td");
        msg.className = "err";
        msg.textContent = row.error;
        var actCell = document.createElement("td");
        var btn = document.createElement("button");
        btn.className = "secondary";
        btn.textContent = "Retry";
        btn.addEventListener("click", function () {
          act(function () { return api("POST", "library/analysis", { ids: [row.id] }); }, "Retrying " + row.title + ".");
        });
        actCell.appendChild(btn);
        tr.append(track, tool, msg, actCell);
        body.appendChild(tr);
      });
      $("retryFailed").disabled = listed.length === 0;
    } catch (e) { say(e.message); }
  }
  async function refresh() {
    try { render(await api("GET", "library/stats")); }
    catch (e) { say(e.message); schedule(10000); }
  }
  async function act(fn, msg) {
    try { await fn(); say(msg); } catch (e) { say(e.message); }
    refresh();
  }

  $("scan").addEventListener("click", function () {
    act(function () { return api("POST", "library/scan"); }, "Rescan started.");
  });
  $("bpmMissingBtn").addEventListener("click", function () {
    act(function () { return api("POST", "library/bpm", { mode: "missing" }); }, "Calculating missing BPM.");
  });
  $("bpmAllBtn").addEventListener("click", function () {
    if (!confirm("Recalculate every calculated BPM and fill in missing ones? Values from file tags are kept. This takes about a second per track.")) return;
    act(function () { return api("POST", "library/bpm", { mode: "all" }); }, "Recalculating BPM.");
  });
  $("retryFailed").addEventListener("click", function () {
    var ids = listed.map(function (r) { return r.id; });
    if (ids.length === 0) return;
    act(function () { return api("POST", "library/analysis", { ids: ids }); }, "Retrying " + ids.length + " tracks.");
  });
  $("stop").addEventListener("click", function () {
    act(function () { return api("POST", "library/jobs/cancel"); }, "Stopping after the current track.");
  });
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) refresh();
  });
  refresh();
})();
