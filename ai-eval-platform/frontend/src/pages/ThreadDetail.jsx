import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, fmt } from "../api.js";

function valueText(v) {
  if (v == null) return "—";
  return typeof v === "string" ? v : JSON.stringify(v, null, 2);
}

function ScorePill({ score }) {
  if (score == null) return <span className="pill">no score</span>;
  const cls = score >= 0.8 ? "good" : score < 0.5 ? "bad" : "";
  return <span className={"pill " + cls}>{Number(score).toFixed(2)}</span>;
}

export default function ThreadDetail() {
  const { pid, thid } = useParams();
  const [d, setD] = useState(null);
  const [scorers, setScorers] = useState([]);
  const [manual, setManual] = useState({ name: "thread_completion", score: "", rationale: "" });
  const [busy, setBusy] = useState(false);
  const [toast, setToast] = useState(null);
  const mounted = useRef(true);

  const load = useCallback(
    () => api.get(`/api/v1/projects/${pid}/threads/${encodeURIComponent(thid)}`).then(setD),
    [pid, thid]
  );
  useEffect(() => {
    mounted.current = true;
    load().catch(() => setD({}));
    api.get("/api/v1/scorers?scope=thread").then((r) => {
      setScorers(r);
      if (r.length) setManual((m) => ({ ...m, name: r[0].name }));
    }).catch(() => {});
    return () => { mounted.current = false; };
  }, [load]);

  const notify = (msg) => {
    setToast(msg);
    setTimeout(() => setToast(null), 3000);
  };

  const act = async (action) => {
    try {
      await api.post(`/api/v1/projects/${pid}/threads/${encodeURIComponent(thid)}/${action}`);
      await load();
      notify(action === "close" ? "Conversation closed" : "Conversation reopened");
    } catch (e) {
      notify("Could not update conversation: " + e.message);
    }
  };

  const evaluate = async () => {
    setBusy(true);
    try {
      const job = await api.post(`/api/v1/projects/${pid}/threads/${encodeURIComponent(thid)}/evaluate`, {
        scorers: scorers.map((s) => s.name),
      });
      const deadline = Date.now() + 90_000;
      while (mounted.current && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 1000));
        const next = await api.get(`/api/v1/projects/${pid}/threads/${encodeURIComponent(thid)}`);
        if (!mounted.current) return;
        setD(next);
        const jobRow = (next.evaluations || []).find((e) => e.job_id === job.job_id);
        if (jobRow && (jobRow.status === "done" || jobRow.status === "failed")) {
          notify(jobRow.status === "done" ? "Conversation scored" : "Conversation evaluation failed: " + (jobRow.last_error || "worker error"));
          return;
        }
      }
      notify("Evaluation is still running; refresh to see scores");
    } catch (e) {
      notify("Could not start evaluation: " + e.message);
    } finally {
      if (mounted.current) setBusy(false);
    }
  };

  const saveScore = async () => {
    if (!manual.name) return notify("Pick a scorer first");
    const score = manual.score === "" ? null : Number(manual.score);
    if (score != null && (Number.isNaN(score) || score < 0 || score > 1)) {
      return notify("Score must be between 0 and 1");
    }
    try {
      await api.post(`/api/v1/projects/${pid}/threads/${encodeURIComponent(thid)}/scores`, {
        name: manual.name,
        score,
        rationale: manual.rationale || "manual review",
        source: "manual",
      });
      setManual((m) => ({ ...m, score: "", rationale: "" }));
      await load();
      notify("Conversation score saved");
    } catch (e) {
      notify("Could not save score: " + e.message);
    }
  };

  if (!d) return <div className="empty">Loading…</div>;
  if (!d.thread) {
    return (
      <div className="empty">
        Conversation not found. <Link to={`/projects/${pid}/conversations`}>Back to conversations</Link>
      </div>
    );
  }

  const th = d.thread;
  const turns = d.turns || [];
  const scores = d.scores || [];
  const summary = d.score_summary || {};
  const evals = d.evaluations || [];
  const summaryRows = Object.entries(summary);
  const scored = scores.filter((s) => s.score != null);
  const overall = scored.length ? scored.reduce((a, s) => a + s.score, 0) / scored.length : null;

  return (
    <div>
      <div className="row"><Link to={`/projects/${pid}/conversations`} style={{ color: "var(--muted)" }}>← Conversations</Link></div>
      <div className="row" style={{ justifyContent: "space-between", alignItems: "flex-start", flexWrap: "wrap", gap: 12 }}>
        <div style={{ minWidth: 320, flex: 1 }}>
          <h1 style={{ whiteSpace: "nowrap" }}>Conversation <span className="mono" style={{ fontSize: 13 }}>{th.id.length > 36 ? th.id.slice(0, 36) + "…" : th.id}</span></h1>
          <p className="sub" style={{ marginBottom: 0 }}>
            {fmt.num(th.turn_count)} turns · started {new Date(th.started_at_ms).toLocaleString()} · last activity {new Date(th.last_activity_ms).toLocaleString()}
          </p>
        </div>
        <div className="row" style={{ flexWrap: "wrap", flexShrink: 0 }}>
          <span className={"pill " + (th.status === "open" ? "ok" : "")}>{th.status}</span>
          <Link className="btn ghost" style={{ whiteSpace: "nowrap" }}
            to={`/projects/${pid}/traces?thread_id=${encodeURIComponent(th.id)}`}>
            All traces
          </Link>
          <button className="small ghost" style={{ whiteSpace: "nowrap" }} onClick={() => act(th.status === "open" ? "close" : "open")}>
            {th.status === "open" ? "Close conversation" : "Reopen conversation"}
          </button>
          <button className="small" style={{ whiteSpace: "nowrap" }} disabled={busy} onClick={evaluate}>
            {busy ? "Evaluating…" : "Evaluate conversation"}
          </button>
        </div>
      </div>

      <div className="grid cols4" style={{ marginTop: 14 }}>
        <div className="panel stat"><div className="label">Turns</div><div className="value">{fmt.num(th.turn_count)}</div><div className="hint">traces in this conversation</div></div>
        <div className="panel stat"><div className="label">Duration</div><div className="value">{fmt.ms(th.duration_ms)}</div><div className="hint">first turn → last activity</div></div>
        <div className="panel stat"><div className="label">Overall score</div><div className="value">{overall == null ? "–" : overall.toFixed(2)}</div><div className="hint">{fmt.num(scored.length)} score{scored.length === 1 ? "" : "s"} recorded</div></div>
        <div className="panel stat"><div className="label">Evaluations</div><div className="value">{fmt.num(evals.length)}</div><div className="hint">{evals.filter((e) => e.status === "done").length} completed</div></div>
      </div>

      <div className="grid cols2" style={{ marginTop: 14, alignItems: "start", gridTemplateColumns: "1.35fr 1fr" }}>
        <div>
          <div className="panel">
            <h2 style={{ marginTop: 0 }}>Turn timeline</h2>
            {!turns.length && <div className="empty">No turns recorded.</div>}
            {turns.map((t, i) => (
              <div className="card" key={t.trace_id} style={{ marginBottom: 10 }}>
                <div className="row" style={{ justifyContent: "space-between" }}>
                  <div className="row">
                    <span className="pill score">turn {i + 1}</span>
                    <b>{t.name}</b>
                    <span className={"pill " + (t.status === "error" ? "error" : "ok")}>{t.status}</span>
                  </div>
                  <div className="kv">
                    <span>{fmt.ms(t.latency_ms)}</span>
                    <span>·</span>
                    <span>{fmt.usd(t.cost_usd)}</span>
                    <span>·</span>
                    <Link to={`/projects/${pid}/traces/${t.trace_id}`}>view trace →</Link>
                  </div>
                </div>
                <div style={{ color: "var(--muted)", fontSize: 11, marginTop: 8 }}>USER</div>
                <pre className="json" style={{ maxHeight: 130, overflow: "auto", marginTop: 2 }}>{valueText(t.input)}</pre>
                <div style={{ color: "var(--muted)", fontSize: 11, marginTop: 4 }}>ASSISTANT</div>
                <pre className="json" style={{ maxHeight: 160, overflow: "auto", marginTop: 2 }}>{valueText(t.output)}</pre>
                {t.error != null && <pre className="json" style={{ borderColor: "rgba(255,107,107,.5)", marginTop: 4 }}>{valueText(t.error)}</pre>}
              </div>
            ))}
          </div>
        </div>

        <div>
          <div className="panel">
            <h2 style={{ marginTop: 0 }}>Conversation scores</h2>
            <table>
              <thead><tr><th>Scorer</th><th>Average</th><th>Samples</th></tr></thead>
              <tbody>
                {summaryRows.map(([name, s]) => (
                  <tr key={name}>
                    <td className="mono">{name}</td>
                    <td><ScorePill score={s.avg} /></td>
                    <td style={{ color: "var(--muted)" }}>{s.count}</td>
                  </tr>
                ))}
                {!summaryRows.length && <tr><td colSpan={3} className="empty">No conversation scores yet.</td></tr>}
              </tbody>
            </table>
          </div>

          <div className="panel" style={{ marginTop: 14 }}>
            <b>Add a manual score</b>
            <div className="row" style={{ marginTop: 10 }}>
              <select value={manual.name} onChange={(e) => setManual({ ...manual, name: e.target.value })} style={{ flex: 1 }}>
                {scorers.map((s) => <option key={s.name} value={s.name}>{s.name}</option>)}
              </select>
              <input type="number" min="0" max="1" step="0.01" placeholder="0–1" style={{ width: 90 }}
                value={manual.score} onChange={(e) => setManual({ ...manual, score: e.target.value })} />
            </div>
            <textarea rows={2} style={{ width: "100%", marginTop: 8 }} placeholder="Why this score? (optional)"
              value={manual.rationale} onChange={(e) => setManual({ ...manual, rationale: e.target.value })} />
            <button className="small" style={{ marginTop: 8 }} onClick={saveScore}>Save score</button>
          </div>

          <div className="panel" style={{ marginTop: 14 }}>
            <b>Recent scores</b>
            <table style={{ marginTop: 8 }}>
              <tbody>
                {scores.slice(0, 12).map((s) => (
                  <tr key={s.id}>
                    <td className="mono" style={{ fontSize: 11.5 }}>{s.name}</td>
                    <td><ScorePill score={s.score} /></td>
                    <td style={{ color: "var(--muted)", fontSize: 11.5 }}>
                      <div>
                        {s.source}
                        {s.judge_mode && s.judge_mode !== "deterministic" && s.judge_mode !== "manual"
                          ? ` · ${s.judge_mode}` : ""}
                      </div>
                      {s.rationale && <div style={{ marginTop: 2 }}>{s.rationale}</div>}
                    </td>
                  </tr>
                ))}
                {!scores.length && <tr><td className="empty">No scores yet. Run an evaluation above.</td></tr>}
              </tbody>
            </table>
          </div>

          <div className="panel" style={{ marginTop: 14 }}>
            <b>Evaluation history</b>
            <table style={{ marginTop: 8 }}>
              <tbody>
                {evals.map((e) => (
                  <tr key={e.job_id}>
                    <td className="mono" style={{ fontSize: 11 }}>{e.job_id.slice(0, 12)}…</td>
                    <td><span className={"pill " + (e.status === "done" ? "good" : e.status === "failed" ? "bad" : "")}>{e.status}</span></td>
                    <td style={{ color: "var(--muted)", fontSize: 11 }}>
                      {new Date(e.created_at).toLocaleString()}
                      {e.last_error && <div style={{ color: "var(--red)" }}>{e.last_error}</div>}
                    </td>
                  </tr>
                ))}
                {!evals.length && <tr><td className="empty">No evaluations yet.</td></tr>}
              </tbody>
            </table>
          </div>
        </div>
      </div>
      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}
