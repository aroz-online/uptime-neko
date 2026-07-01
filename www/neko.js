/* Uptime Neko — admin UI logic (custom, Uptime Kuma inspired) */

let monitorsCache = [];
let notificationsCache = [];
let pagesCache = [];
let selectedId = null;
let detailRange = "1h";
let detailChart = null;
let refreshTimer = null;
let currentView = "dashboard";

// ---------- CSRF + API helpers ----------
function csrf() {
    const m = document.querySelector('meta[name="zoraxy.csrf.Token"]');
    return m ? m.getAttribute("content") : "";
}
function apiGet(path) {
    return $.ajax({ url: "." + path, type: "GET", dataType: "json" });
}
function apiPostJSON(path, obj) {
    return $.ajax({
        url: "." + path, type: "POST",
        contentType: "application/json",
        headers: { "X-CSRF-Token": csrf() },
        data: JSON.stringify(obj || {}),
        dataType: "json"
    });
}
function apiPost(path) {
    return $.ajax({ url: "." + path, type: "POST", headers: { "X-CSRF-Token": csrf() }, dataType: "json" });
}

// ---------- Theme (driven by Zoraxy; the main Zoraxy UI's own toggle calls
// window.setDarkTheme() on this iframe's contentWindow, so no in-plugin toggle is needed) ----------
function setDarkTheme(isDark) {
    document.body.classList.toggle("darkTheme", !!isDark);
    document.documentElement.classList.toggle("darkTheme", !!isDark);
    localStorage.setItem("theme", isDark ? "dark" : "light");
    if (detailChart && selectedId) loadDetail(true); // recolor chart (recreate)
}
// init theme from shared localStorage before paint
if (localStorage.getItem("theme") === "dark") {
    document.documentElement.classList.add("darkTheme");
    document.body.classList.add("darkTheme");
}

// ---------- toast ----------
let toastTimer = null;
function toast(msg, ok) {
    const t = $("#toast");
    t.text(msg).toggleClass("err", ok === false).addClass("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.removeClass("show"); }, 3000);
}
function escapeHtml(s) { return $("<div>").text(s == null ? "" : s).html(); }
function errMsg(xhr) { try { return (xhr.responseJSON && xhr.responseJSON.error) || xhr.statusText; } catch (e) { return "error"; } }

// ---------- init ----------
$(function () {
    setDarkTheme(localStorage.getItem("theme") === "dark");
    loadDashboard();
    loadNotifications();
    loadSettingsHint();
    // close modal on overlay click (cancels the confirm dialog if that's what's open)
    $(".modal-overlay").on("mousedown", function (e) {
        if (e.target !== this) return;
        if (this.id === "confirmModal") { _confirmFinish($("#confirmInputWrap").is(":visible") ? null : false); }
        else closeModal(this.id);
    });
    $("#confirmOkBtn").on("click", function () {
        _confirmFinish($("#confirmInputWrap").is(":visible") ? $("#confirmInput").val() : true);
    });
    $("#confirmCancelBtn").on("click", function () {
        _confirmFinish($("#confirmInputWrap").is(":visible") ? null : false);
    });
    $("#confirmInput").on("keydown", function (e) {
        if (e.key === "Enter") $("#confirmOkBtn").trigger("click");
    });
    startAutoRefresh();
});

function startAutoRefresh() {
    if (refreshTimer) clearInterval(refreshTimer);
    refreshTimer = setInterval(function () {
        if (currentView === "dashboard" && !$(".modal-overlay.show").length) loadDashboard(true);
    }, 5000);
}

// ---------- view switching ----------
function switchView(v) {
    currentView = v;
    $(".nav-link").removeClass("active");
    $('.nav-link[data-view="' + v + '"]').addClass("active");
    $(".view").removeClass("active");
    $('.view[data-view="' + v + '"]').addClass("active");
    if (v === "dashboard") loadDashboard();
    if (v === "statuspages") loadPages();
    if (v === "notifications") loadNotifications(renderNotifyTable);
    if (v === "tokens") loadTokens();
    if (v === "settings") loadSettings();
}

// ---------- modal helpers ----------
function openModal(id) { $("#" + id).addClass("show"); }
function closeModal(id) { $("#" + id).removeClass("show"); }

// ---------- confirm / prompt dialog ----------
// Zoraxy's plugin iframe is sandboxed without allow-modals, so native
// confirm()/prompt() are silently blocked (they return false/null instantly,
// before any user interaction). This is an in-app replacement, promise-based.
let _confirmResolve = null;
function showConfirm(message, opts) {
    opts = opts || {};
    return new Promise(function (resolve) {
        _confirmResolve = resolve;
        $("#confirmTitle").text(opts.title || (opts.isPrompt ? "Input Required" : "Please Confirm"));
        $("#confirmMessage").text(message);
        $("#confirmOkBtn").removeClass("btn-green btn-red").addClass(opts.danger ? "btn-red" : "btn-green").text(opts.okText || "OK");
        if (opts.isPrompt) {
            $("#confirmInputWrap").show();
            $("#confirmInput").val(opts.defaultValue || "");
        } else {
            $("#confirmInputWrap").hide();
        }
        openModal("confirmModal");
        if (opts.isPrompt) setTimeout(function () { $("#confirmInput").trigger("focus").select(); }, 50);
    });
}
function _confirmFinish(value) {
    closeModal("confirmModal");
    if (_confirmResolve) { _confirmResolve(value); _confirmResolve = null; }
}

// ---------- Dashboard ----------
function loadDashboard(silent) {
    apiGet("/api/summary").done(function (data) {
        monitorsCache = data || [];
        renderSidebar();
        if (selectedId && monitorsCache.find(function (m) { return m.id === selectedId; })) {
            if (!silent) renderDetail();
            else updateDetailLive();
        } else if (!selectedId) {
            renderQuickStats();
        }
    }).fail(function () {
        if (!silent) $("#monList").html('<div class="empty">Failed to load.</div>');
    });
}

function monById(id) { return monitorsCache.find(function (m) { return m.id === id; }); }

function statusOf(m) {
    if (!m.enabled) return "paused";
    if (!m.known) return "unknown";
    return m.current_up ? "up" : "down";
}

function uptimePct(v) { return (v == null || v < 0) ? "—" : (v * 100).toFixed(2) + "%"; }

// uptime color tiers: >=90% green, <90% yellow, <30% red. Returns "" (no data) when unknown.
function uptimeTier(v) {
    if (v == null || v < 0) return "";
    if (v >= 0.90) return "green";
    if (v >= 0.30) return "warn";
    return "red";
}
// same tiers, but for the sidebar pill where the default (unclassed) look is already green
function pillTier(v) {
    const t = uptimeTier(v);
    return t === "green" ? "" : t;
}

function renderBars(heartbeats, max, cls) {
    const hb = (heartbeats || []).slice(-(max || 40));
    let html = '<div class="bars ' + (cls || "") + '">';
    for (let i = 0; i < hb.length; i++) {
        const b = hb[i];
        let h = 10;
        if (b.up && b.latency > 0) h = Math.min(30, 8 + Math.log10(b.latency + 1) * 9);
        else if (!b.up) h = 30;
        html += '<div class="bar ' + (b.up ? "up" : "down") + '" style="height:' + h.toFixed(0) +
            'px" title="' + escapeHtml(fmtTime(b.time) + " · " + b.msg) + '"></div>';
    }
    return html + "</div>";
}

function renderSidebar() {
    const q = ($("#monSearch").val() || "").toLowerCase();
    const list = monitorsCache.filter(function (m) { return m.name.toLowerCase().indexOf(q) >= 0; });
    const c = $("#monList");
    if (!monitorsCache.length) {
        c.html('<div class="empty">No monitors yet.<br>Click “Add New Monitor”.</div>');
        return;
    }
    c.empty();
    list.forEach(function (m) {
        const st = statusOf(m);
        const showPct = st === "up" || st === "down";
        const pillCls = showPct ? (pillTier(m.uptime_24h) || "") : "paused";
        const pillTxt = st === "paused" ? "Paused" : (st === "unknown" ? "—" : uptimePct(m.uptime_24h));
        const el = $(
            '<div class="mon-item' + (m.id === selectedId ? " active" : "") + '" data-id="' + m.id + '">' +
            '<div class="mon-item-top"><span class="pill ' + pillCls + '">' + pillTxt + '</span>' +
            '<span class="mon-name">' + escapeHtml(m.name) + '</span></div>' +
            renderBars(m.heartbeats, 30, "sm") +
            '</div>'
        );
        el.on("click", function () { selectMonitor(m.id); });
        c.append(el);
    });
}

function selectMonitor(id) {
    selectedId = id;
    detailRange = "1h";
    renderSidebar();
    renderDetail();
}

// ---------- Quick stats (no selection) ----------
function renderQuickStats() {
    let up = 0, down = 0, paused = 0, unknown = 0;
    monitorsCache.forEach(function (m) {
        const s = statusOf(m);
        if (s === "up") up++; else if (s === "down") down++; else if (s === "paused") paused++; else unknown++;
    });
    // recent events from heartbeat transitions
    const events = [];
    monitorsCache.forEach(function (m) {
        const hb = m.heartbeats || [];
        for (let i = 1; i < hb.length; i++) {
            if (hb[i].up !== hb[i - 1].up) events.push({ name: m.name, up: hb[i].up, time: hb[i].time, msg: hb[i].msg });
        }
    });
    events.sort(function (a, b) { return b.time - a.time; });

    let html =
        '<div class="quick-grid">' +
        quickCard(up, "Up", "green") + quickCard(down, "Down", "red") +
        quickCard(paused, "Paused", "") + quickCard(unknown, "Unknown", "") +
        quickCard(monitorsCache.length, "Total", "") + '</div>';

    html += '<div class="panel"><div class="panel-pad" style="padding-bottom:.3rem;"><div class="sec-title">Recent Events</div></div>';
    if (!events.length) {
        html += '<div class="empty">No events recorded yet. Select a monitor from the left to see details.</div></div>';
    } else {
        html += '<table class="tbl"><thead><tr><th>Monitor</th><th>Status</th><th>Time</th><th>Message</th></tr></thead><tbody>';
        events.slice(0, 20).forEach(function (e) {
            html += '<tr><td>' + escapeHtml(e.name) + '</td>' +
                '<td><span class="tag ' + (e.up ? "up" : "down") + '">' + (e.up ? "Up" : "Down") + '</span></td>' +
                '<td>' + fmtDateTime(e.time) + '</td><td>' + escapeHtml(e.msg) + '</td></tr>';
        });
        html += '</tbody></table></div>';
    }
    $("#mainPane").html(html);
}
function quickCard(v, l, cls) {
    return '<div class="panel quick"><div class="v ' + cls + '">' + v + '</div><div class="l">' + l + '</div></div>';
}

// ---------- Monitor detail ----------
function renderDetail() {
    const m = monById(selectedId);
    if (!m) { selectedId = null; renderQuickStats(); return; }
    const st = statusOf(m);
    const badgeTxt = st === "up" ? "Up" : (st === "down" ? "Down" : (st === "paused" ? "Paused" : "Pending"));
    const ping = m.last_latency ? m.last_latency.toFixed(1) + " ms" : "—";

    let certLine = "";
    if (m.cert_expiry) {
        const days = Math.round((m.cert_expiry - Date.now()) / 86400000);
        certLine = ' · <span class="icon icon-lock"></span> cert expires in ' + days + ' days';
    }

    const html =
        '<div class="panel panel-pad">' +
        '<div class="detail-head">' +
        '<div><div class="detail-title">' + escapeHtml(m.name) + '</div>' +
        '<div class="detail-sub">' + escapeHtml(m.type.toUpperCase() + ": " + m.target) + certLine + '</div></div>' +
        '<div class="detail-actions">' +
        '<button class="btn btn-grey btn-sm" onclick="toggleMonitor(\'' + m.id + '\')">' + (m.enabled ? '<span class="icon icon-pause"></span> Pause' : '<span class="icon icon-play"></span> Resume') + '</button>' +
        '<button class="btn btn-grey btn-sm" onclick="openMonitorModal(\'' + m.id + '\')"><span class="icon icon-edit"></span> Edit</button>' +
        '<button class="btn btn-grey btn-sm" onclick="cloneMonitor(\'' + m.id + '\')"><span class="icon icon-clone"></span> Clone</button>' +
        '<button class="btn btn-red btn-sm" onclick="deleteMonitor(\'' + m.id + '\')"><span class="icon icon-delete"></span> Delete</button>' +
        '</div></div>' +

        '<div class="bigbars-row"><span id="dBars">' + renderBars(m.heartbeats, 50, "") + '</span>' +
        '<span class="status-badge ' + st + '" id="dBadge">' + badgeTxt + '</span></div>' +
        '<div class="check-interval" id="dCheck">Check every ' + m.interval + ' seconds' + (m.last_message ? ' · ' + escapeHtml(m.last_message) : "") + '</div>' +

        '<div class="stat-grid">' +
        '<div class="stat"><div class="v" id="dCurrent">' + ping + '</div><div class="l">Current Ping</div></div>' +
        '<div class="stat"><div class="v" id="dAvg">—</div><div class="l">Avg Ping (24h)</div></div>' +
        '<div class="stat"><div class="v ' + uptimeTier(m.uptime_24h) + '" id="dUp24">' + uptimePct(m.uptime_24h) + '</div><div class="l">Uptime (24h)</div></div>' +
        '<div class="stat"><div class="v ' + uptimeTier(m.uptime_30d) + '" id="dUp30">' + uptimePct(m.uptime_30d) + '</div><div class="l">Uptime (30d)</div></div>' +
        '</div>' +

        '<div class="range-tabs">' +
        ["1h", "6h", "24h", "7d", "30d"].map(function (r) {
            return '<button class="' + (r === detailRange ? "active" : "") + '" onclick="setRange(\'' + r + '\')">' + r + '</button>';
        }).join("") + '</div>' +
        '<div class="chart-wrap"><canvas id="detailChart"></canvas></div>' +
        '</div>';
    $("#mainPane").html(html);
    detailChart = null;
    loadDetail(true);
}

// light update during silent auto-refresh: patch numbers/bars in place and
// update the chart without rebuilding the DOM (so the chart does not re-animate).
function updateDetailLive() {
    const m = monById(selectedId);
    if (!m || !$("#detailChart").length) { renderDetail(); return; }
    const st = statusOf(m);
    const badgeTxt = st === "up" ? "Up" : (st === "down" ? "Down" : (st === "paused" ? "Paused" : "Pending"));
    $("#dBars").html(renderBars(m.heartbeats, 50, ""));
    $("#dBadge").attr("class", "status-badge " + st).text(badgeTxt);
    $("#dCheck").text("Check every " + m.interval + " seconds" + (m.last_message ? " · " + m.last_message : ""));
    $("#dCurrent").text(m.last_latency ? m.last_latency.toFixed(1) + " ms" : "—");
    $("#dUp24").attr("class", "v " + uptimeTier(m.uptime_24h)).text(uptimePct(m.uptime_24h));
    $("#dUp30").attr("class", "v " + uptimeTier(m.uptime_30d)).text(uptimePct(m.uptime_30d));
    loadDetail(false);
}

function setRange(r) {
    detailRange = r;
    $(".range-tabs button").removeClass("active");
    $(".range-tabs button").filter(function () { return $(this).text() === r; }).addClass("active");
    loadDetail(true); // range change resets the axis -> recreate
}

// loadDetail(recreate): recreate=true builds a fresh chart (with intro animation);
// recreate=false updates the existing chart's data with no animation.
function loadDetail(recreate) {
    if (!selectedId) return;
    apiGet("/api/monitor/detail?id=" + encodeURIComponent(selectedId) + "&range=" + detailRange).done(function (d) {
        $("#dAvg").text(d.avg_ping ? d.avg_ping.toFixed(1) + " ms" : "—");
        applyChart(d.heartbeats || [], recreate);
    });
}

function chartArrays(beats) {
    return {
        labels: beats.map(function (b) { return fmtTime(b.time); }),
        data: beats.map(function (b) { return b.up ? b.latency : null; }),
        points: beats.map(function (b) { return b.up ? "#3ba55c" : "#dc3545"; })
    };
}

function applyChart(beats, recreate) {
    const cv = document.getElementById("detailChart");
    if (!cv) return;
    if (recreate || !detailChart) { renderChart(beats); return; }
    // update in place: replace data and redraw with NO animation (just shows the
    // latest points from the right; the line no longer slides up on each refresh)
    const a = chartArrays(beats);
    detailChart.data.labels = a.labels;
    detailChart.data.datasets[0].data = a.data;
    detailChart.data.datasets[0].pointBackgroundColor = a.points;
    detailChart.data.datasets[0].pointRadius = beats.length > 80 ? 0 : 2;
    detailChart.update("none");
}

function renderChart(beats) {
    const cv = document.getElementById("detailChart");
    if (!cv) return;
    if (detailChart) detailChart.destroy();
    const ctx = cv.getContext("2d");
    const textColor = getComputedStyle(document.body).getPropertyValue("--muted") || "#888";
    const grad = ctx.createLinearGradient(0, 0, 0, 300);
    grad.addColorStop(0, "rgba(92,221,139,0.35)");
    grad.addColorStop(1, "rgba(92,221,139,0.02)");
    const a = chartArrays(beats);
    detailChart = new Chart(ctx, {
        type: "line",
        data: {
            labels: a.labels,
            datasets: [{
                data: a.data,
                borderColor: "#3ba55c", backgroundColor: grad,
                pointBackgroundColor: a.points,
                pointRadius: beats.length > 80 ? 0 : 2, borderWidth: 2, tension: 0.35, fill: true, spanGaps: true
            }]
        },
        options: {
            responsive: true, maintainAspectRatio: false,
            animation: { duration: 350 }, // intro only; live updates use update("none")
            interaction: { intersect: false, mode: "index" },
            plugins: { legend: { display: false } },
            scales: {
                x: { ticks: { color: textColor, maxTicksLimit: 8, autoSkip: true }, grid: { display: false } },
                y: { beginAtZero: true, ticks: { color: textColor }, grid: { color: "rgba(128,128,128,0.15)" } }
            }
        }
    });
}

// ---------- Monitor modal ----------
function onTypeChange() {
    const t = $("#mType").val();
    $("[data-type]").each(function () {
        const types = ($(this).attr("data-type") || "").split(" ");
        $(this).css("display", types.indexOf(t) >= 0 ? "" : "none");
    });
    const labels = {
        http: ["URL", "https://example.com"], ping: ["Hostname or IP", "1.1.1.1"],
        tcp: ["Host", "192.168.1.1"], dns: ["Hostname to resolve", "example.com"],
        ssl: ["Hostname", "example.com"]
    };
    $("#mTargetLabel").text(labels[t][0]);
    $("#mTarget").attr("placeholder", labels[t][1]);
}

function openMonitorModal(id, asClone) {
    $("#mId").val(""); $("#mName").val(""); $("#mTarget").val(""); $("#mPort").val("");
    $("#mKeyword").val(""); $("#mDnsResolver").val(""); $("#mDnsExpected").val("");
    $("#mKeywordInvert,#mIgnoreTLS").prop("checked", false);
    $("#mExpectedStatus").val(0); $("#mCertWarnHttp").val(0); $("#mCertWarnSsl").val(14);
    $("#mInterval").val(20); $("#mTimeout").val(10); $("#mEnabled").prop("checked", true);
    $("#mType").val("http"); $("#mMethod").val("GET"); $("#mDnsType").val("A");

    const m = id ? monById(id) : null;
    renderNotifyChecklist("#mNotifyChecklist", m ? m.notification_ids : []);
    if (m) {
        $("#monitorModalTitle").text(asClone ? "Clone Monitor" : "Edit Monitor");
        if (!asClone) $("#mId").val(m.id);
        $("#mName").val(asClone ? m.name + " (copy)" : m.name);
        $("#mType").val(m.type); $("#mTarget").val(m.target);
        $("#mInterval").val(m.interval); $("#mTimeout").val(m.timeout); $("#mPort").val(m.port || "");
        $("#mMethod").val(m.method || "GET"); $("#mExpectedStatus").val(m.expected_status || 0);
        $("#mKeyword").val(m.keyword || ""); $("#mKeywordInvert").prop("checked", !!m.keyword_invert);
        $("#mIgnoreTLS").prop("checked", !!m.ignore_tls); $("#mCertWarnHttp").val(m.cert_warn_days || 0);
        $("#mCertWarnSsl").val(m.cert_warn_days || 14);
        $("#mDnsType").val(m.dns_record_type || "A"); $("#mDnsResolver").val(m.dns_resolver || "");
        $("#mDnsExpected").val(m.dns_expected || ""); $("#mEnabled").prop("checked", m.enabled);
    } else {
        $("#monitorModalTitle").text("Add Monitor");
    }
    onTypeChange();
    openModal("monitorModal");
}
function cloneMonitor(id) { openMonitorModal(id, true); }

function saveMonitor() {
    const t = $("#mType").val();
    const m = {
        id: $("#mId").val(), name: $("#mName").val().trim(), type: t, target: $("#mTarget").val().trim(),
        interval: parseInt($("#mInterval").val()) || 20, timeout: parseInt($("#mTimeout").val()) || 10,
        enabled: $("#mEnabled").is(":checked"), notification_ids: getCheckedIds("#mNotifyChecklist")
    };
    if (!m.name || !m.target) { toast("Name and target are required", false); return; }
    if (t === "http") {
        m.method = $("#mMethod").val(); m.expected_status = parseInt($("#mExpectedStatus").val()) || 0;
        m.keyword = $("#mKeyword").val(); m.keyword_invert = $("#mKeywordInvert").is(":checked");
        m.ignore_tls = $("#mIgnoreTLS").is(":checked"); m.cert_warn_days = parseInt($("#mCertWarnHttp").val()) || 0;
    } else if (t === "tcp") { m.port = parseInt($("#mPort").val()) || 0; }
    else if (t === "ssl") { m.port = parseInt($("#mPort").val()) || 0; m.cert_warn_days = parseInt($("#mCertWarnSsl").val()) || 0; }
    else if (t === "dns") { m.dns_record_type = $("#mDnsType").val(); m.dns_resolver = $("#mDnsResolver").val(); m.dns_expected = $("#mDnsExpected").val(); }

    apiPostJSON(m.id ? "/api/monitor/update" : "/api/monitor/add", m).done(function (saved) {
        closeModal("monitorModal");
        if (saved && saved.id && !m.id) selectedId = saved.id;
        loadDashboard();
        toast("Monitor saved");
    }).fail(function (xhr) { toast("Save failed: " + errMsg(xhr), false); });
}

function toggleMonitor(id) { apiPost("/api/monitor/toggle?id=" + encodeURIComponent(id)).done(function () { loadDashboard(); }); }

async function deleteMonitor(id) {
    const m = monById(id);
    const ok = await showConfirm('Delete monitor "' + (m ? m.name : "") + '" and all its history?', { danger: true, okText: "Delete" });
    if (!ok) return;
    apiPost("/api/monitor/delete?id=" + encodeURIComponent(id)).done(function () {
        if (selectedId === id) selectedId = null;
        loadDashboard(); toast("Monitor deleted");
    });
}

// ---------- Notifications ----------
function loadNotifications(cb) { apiGet("/api/notify/list").done(function (d) { notificationsCache = d || []; if (cb) cb(); }); }
function renderNotifyTable() {
    const tb = $("#notifyList").empty();
    if (!notificationsCache.length) { tb.append('<tr><td colspan="4" class="empty">No notification channels.</td></tr>'); return; }
    notificationsCache.forEach(function (n) {
        tb.append('<tr><td>' + escapeHtml(n.name) + '</td><td>' + n.type.toUpperCase() + '</td>' +
            '<td>' + (n.enabled ? '<span class="icon icon-check icon-good"></span>' : '<span class="icon icon-close icon-bad"></span>') + '</td>' +
            '<td><div class="row-actions"><button class="btn btn-grey btn-sm" onclick="openNotifyModal(\'' + n.id + '\')"><span class="icon icon-edit"></span></button>' +
            '<button class="btn btn-red btn-sm" onclick="deleteNotify(\'' + n.id + '\')"><span class="icon icon-delete"></span></button></div></td></tr>');
    });
}
function openNotifyModal(id) {
    const n = id ? notificationsCache.find(function (x) { return x.id === id; }) : null;
    $("#nId").val(""); $("#nName").val(""); $("#nWhUrl").val("");
    $("#nEnabled").prop("checked", true); $("#nWhMethod").val("POST");
    if (n) {
        $("#notifyModalTitle").text("Edit Notification"); $("#nId").val(n.id); $("#nName").val(n.name);
        $("#nWhUrl").val(n.webhook_url || ""); $("#nWhMethod").val(n.webhook_method || "POST"); $("#nEnabled").prop("checked", n.enabled);
    } else { $("#notifyModalTitle").text("Add Notification"); }
    openModal("notifyModal");
}
function gatherNotify() {
    return {
        id: $("#nId").val(), name: $("#nName").val().trim(), type: "webhook", enabled: $("#nEnabled").is(":checked"),
        webhook_url: $("#nWhUrl").val(), webhook_method: $("#nWhMethod").val()
    };
}
function saveNotify() {
    const n = gatherNotify();
    if (!n.name) { toast("Name is required", false); return; }
    apiPostJSON("/api/notify/save", n).done(function () { closeModal("notifyModal"); loadNotifications(renderNotifyTable); toast("Notification saved"); })
        .fail(function (xhr) { toast("Save failed: " + errMsg(xhr), false); });
}
function testNotify() {
    toast("Sending test...");
    apiPostJSON("/api/notify/test", gatherNotify()).done(function () { toast("Test sent successfully"); })
        .fail(function (xhr) { toast("Test failed: " + errMsg(xhr), false); });
}
async function deleteNotify(id) {
    const ok = await showConfirm("Delete this notification channel?", { danger: true, okText: "Delete" });
    if (!ok) return;
    apiPost("/api/notify/delete?id=" + encodeURIComponent(id)).done(function () { loadNotifications(renderNotifyTable); });
}
function renderNotifyChecklist(sel, selectedIds) {
    selectedIds = selectedIds || [];
    const c = $(sel).empty();
    if (!notificationsCache.length) { c.html('<span class="hint">No notification channels yet. Add some in the Notifications tab.</span>'); return; }
    notificationsCache.forEach(function (n) {
        const checked = selectedIds.indexOf(n.id) >= 0 ? "checked" : "";
        c.append('<label class="checkrow"><input type="checkbox" value="' + n.id + '" ' + checked + '> ' + escapeHtml(n.name) + ' (' + n.type + ')</label>');
    });
}
function getCheckedIds(sel) {
    const ids = []; $(sel).find('input[type="checkbox"]:checked').each(function () { ids.push($(this).val()); }); return ids;
}

// ---------- Status pages ----------
function loadPages() {
    apiGet("/api/statuspage/list").done(function (data) {
        pagesCache = data || [];
        const tb = $("#pageList").empty();
        if (!pagesCache.length) { tb.append('<tr><td colspan="6" class="empty">No status pages.</td></tr>'); return; }
        pagesCache.forEach(function (p) {
            tb.append('<tr><td>' + escapeHtml(p.title) + (p.is_default ? ' <span class="pill">default</span>' : "") + '</td>' +
                '<td>' + escapeHtml(p.slug) + '</td><td>' + escapeHtml((p.hostnames || []).join(", ")) + '</td>' +
                '<td>' + (p.monitor_ids || []).length + '</td><td>' + (p.password_hash ? '<span class="icon icon-shieldlock"></span>' : "—") + '</td>' +
                '<td><div class="row-actions"><button class="btn btn-grey btn-sm" onclick="openPageModal(\'' + p.id + '\')"><span class="icon icon-edit"></span></button>' +
                '<button class="btn btn-red btn-sm" onclick="deletePage(\'' + p.id + '\')"><span class="icon icon-delete"></span></button></div></td></tr>');
        });
    });
}
function openPageModal(id) {
    const p = id ? pagesCache.find(function (x) { return x.id === id; }) : null;
    $("#pId").val(""); $("#pTitle,#pSlug,#pDesc,#pHosts,#pPassword").val(""); $("#pClearPasswd,#pDefault").prop("checked", false); $("#pTheme").val("auto");
    const c = $("#pMonitorChecklist").empty();
    if (!monitorsCache.length) { c.html('<span class="hint">No monitors yet.</span>'); }
    else {
        const selected = p ? (p.monitor_ids || []) : [];
        monitorsCache.forEach(function (m) {
            const checked = selected.indexOf(m.id) >= 0 ? "checked" : "";
            c.append('<label class="checkrow"><input type="checkbox" value="' + m.id + '" ' + checked + '> ' + escapeHtml(m.name) + '</label>');
        });
    }
    if (p) {
        $("#pageModalTitle").text("Edit Status Page"); $("#pId").val(p.id); $("#pTitle").val(p.title); $("#pSlug").val(p.slug);
        $("#pDesc").val(p.description); $("#pHosts").val((p.hostnames || []).join(", ")); $("#pTheme").val(p.theme || "auto"); $("#pDefault").prop("checked", !!p.is_default);
    } else { $("#pageModalTitle").text("Add Status Page"); }
    openModal("pageModal");
}
function savePage() {
    const hosts = $("#pHosts").val().split(",").map(function (s) { return s.trim(); }).filter(Boolean);
    const p = {
        id: $("#pId").val(), title: $("#pTitle").val().trim(), slug: $("#pSlug").val().trim(), description: $("#pDesc").val(),
        hostnames: hosts, theme: $("#pTheme").val(), is_default: $("#pDefault").is(":checked"),
        monitor_ids: getCheckedIds("#pMonitorChecklist"), password: $("#pPassword").val(), clear_passwd: $("#pClearPasswd").is(":checked")
    };
    if (!p.title || !p.slug) { toast("Title and slug are required", false); return; }
    apiPostJSON("/api/statuspage/save", p).done(function () { closeModal("pageModal"); loadPages(); toast("Status page saved"); })
        .fail(function (xhr) { toast("Save failed: " + errMsg(xhr), false); });
}
async function deletePage(id) {
    const ok = await showConfirm("Delete this status page?", { danger: true, okText: "Delete" });
    if (!ok) return;
    apiPost("/api/statuspage/delete?id=" + encodeURIComponent(id)).done(function () { loadPages(); });
}

// ---------- Tokens ----------
function loadTokens() {
    apiGet("/api/token/list").done(function (data) {
        const tb = $("#tokenList").empty();
        if (!data || !data.length) { tb.append('<tr><td colspan="3" class="empty">No API tokens.</td></tr>'); return; }
        data.forEach(function (t) {
            tb.append('<tr><td>' + escapeHtml(t.name) + '</td><td>' + new Date(t.created * 1000).toLocaleString() + '</td>' +
                '<td><div class="row-actions"><button class="btn btn-red btn-sm" onclick="deleteToken(\'' + t.id + '\')"><span class="icon icon-delete"></span></button></div></td></tr>');
        });
    });
}
async function createToken() {
    const name = await showConfirm("Name this API token:", { isPrompt: true, defaultValue: "my-token", okText: "Create" });
    if (name === null) return;
    apiPostJSON("/api/token/create", { name: name || "token" }).done(function (d) { $("#tokenValue").val(d.token); openModal("tokenModal"); loadTokens(); })
        .fail(function (xhr) { toast("Create failed: " + errMsg(xhr), false); });
}
function copyToken() {
    const el = document.getElementById("tokenValue"); el.select();
    try { document.execCommand("copy"); toast("Copied to clipboard"); } catch (e) { toast("Copy failed", false); }
}
async function deleteToken(id) {
    const ok = await showConfirm("Revoke this token?", { danger: true, okText: "Revoke" });
    if (!ok) return;
    apiPost("/api/token/delete?id=" + encodeURIComponent(id)).done(function () { loadTokens(); });
}

// ---------- Settings ----------
function loadSettings() {
    apiGet("/api/settings/get").done(function (s) {
        $("#setBindAddr").val(s.public_bind_addr); $("#setPublicPort").val(s.public_port);
        $("#setInterval").val(s.default_interval); $("#setRetention").val(s.retention_days);
    });
}
function loadSettingsHint() {
    apiGet("/api/settings/get").done(function (s) { $("#publicHint").text((s.public_bind_addr || "127.0.0.1") + ":" + s.public_port); });
}
function saveSettings() {
    const body = {
        public_bind_addr: $("#setBindAddr").val().trim(), public_port: parseInt($("#setPublicPort").val()) || 0,
        default_interval: parseInt($("#setInterval").val()) || 20, retention_days: parseInt($("#setRetention").val()) || 30
    };
    apiPostJSON("/api/settings/save", body).done(function (r) {
        toast("Settings saved"); loadSettingsHint(); if (r.restart_required) $("#restartNotice").show();
    }).fail(function (xhr) { toast("Save failed: " + errMsg(xhr), false); });
}

// ---------- utils ----------
function fmtTime(ms) { const d = new Date(ms); return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0"); }
function fmtDateTime(ms) { return new Date(ms).toLocaleString(); }
