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
    $("jobLast").hidden = jobRunning || !job.kind;
    if (!jobRunning && job.kind) {
      $("jobLast").textContent = "Last run (" + (job.mode === "all" ? "recalculate" : "missing") + "), finished " +
        when(job.finishedAt) + ": " + num(job.updated) + " stored, " + num(job.failed) + " failed" +
        (job.error ? " — " + job.error : "") + ".";
    }

    clearTimeout(timer);
    timer = setTimeout(refresh, busy ? 2000 : 30000);
  }
  async function refresh() {
    try { render(await api("GET", "library/stats")); }
    catch (e) { say(e.message); timer = setTimeout(refresh, 10000); }
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
  $("stop").addEventListener("click", function () {
    act(function () { return api("POST", "library/jobs/cancel"); }, "Stopping after the current track.");
  });
  refresh();
})();
