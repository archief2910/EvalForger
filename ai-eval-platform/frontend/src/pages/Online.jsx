import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../api.js";

// Online evaluation is the continuous half of the loop: a rule watches live
// traffic and scores it without anyone promoting a trace to a dataset first.
// The page is deliberately two tables — what is watching, and what fired.

const EMPTY = {
  name: "", scorers: ["exact_match"], sample_rate: 1, alert_threshold: 0.5,
  filter_status: "", filter_model: "", filter_name: "",
};

function filterSummary(rule) {
  const f = rule.filter || {};
  const parts = [];
  if (f.status) parts.push(`status=${f.status}`);
  if (f.model) parts.push(`model=${f.model}`);
  if (f.name) parts.push(`name~${f.name}`);
  return parts.length ? parts.join(" · ") : "every trace";
}

export default function Online() {
  const { pid } = useParams();
  const [rules, setRules] = useState([]);
  const [alerts, setAlerts] = useState([]);
  const [scorers, setScorers] = useState([]);
  const [form, setForm] = useState(EMPTY);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");

  const load = () => {
    api.get(`/api/v1/projects/${pid}/online-rules`).then(setRules).catch(() => setRules([]));
    api.get(`/api/v1/projects/${pid}/alerts?status=open`).then((d) => setAlerts(d.alerts || [])).catch(() => setAlerts([]));
  };
  useEffect(() => {
    load();
    api.get(`/api/v1/scorers?scope=record`).then(setScorers).catch(() => setScorers([]));
  }, [pid]);

  const toggleScorer = (name) => {
    setForm((f) => ({
      ...f,
      scorers: f.scorers.includes(name) ? f.scorers.filter((s) => s !== name) : [...f.scorers, name],
    }));
  };

  const create = async () => {
    setError("");
    try {
      await api.post(`/api/v1/projects/${pid}/online-rules`, {
        ...form,
        sample_rate: Number(form.sample_rate),
        alert_threshold: Number(form.alert_threshold) || 0,
      });
      setForm(EMPTY);
      setCreating(false);
      load();
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const toggleEnabled = async (rule) => {
    await api.patch(`/api/v1/projects/${pid}/online-rules/${rule.id}`, { enabled: !rule.enabled });
    load();
  };

  const remove = async (rule) => {
    await api.del(`/api/v1/projects/${pid}/online-rules/${rule.id}`);
    load();
  };

  const ack = async (alert) => {
    await api.post(`/api/v1/projects/${pid}/alerts/${alert.id}/ack`);
    load();
  };

  return (
    <div>
      <h1>Online evaluation</h1>
      <p className="sub">
        Rules score live traffic as it arrives — the continuous counterpart to dataset experiments.
        A rule matches on the trace root, samples a fraction of matches, runs the selected scorers and
        raises an alert when a score falls below its threshold.
      </p>

      <div className="toolbar">
        <button className="small" onClick={() => setCreating((v) => !v)}>
          {creating ? "Cancel" : "+ New rule"}
        </button>
        <div className="spacer" />
        <span style={{ color: "var(--muted)" }}>{alerts.length} open alert{alerts.length === 1 ? "" : "s"}</span>
      </div>

      {creating && (
        <div className="panel" style={{ marginBottom: 14 }}>
          <b>New online rule</b>
          {error && <div style={{ color: "var(--red)", fontSize: 12, marginTop: 6 }}>{error}</div>}
          <div className="grid cols2" style={{ marginTop: 10, alignItems: "start" }}>
            <div style={{ display: "grid", gap: 8 }}>
              <input placeholder="Rule name (e.g. hallucination watch)" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} />
              <div className="row">
                <input style={{ width: 110 }} type="number" step="0.05" min="0" max="1"
                  title="Fraction of matching traces to score"
                  value={form.sample_rate}
                  onChange={(e) => setForm({ ...form, sample_rate: e.target.value })} />
                <span style={{ color: "var(--muted)", fontSize: 12 }}>sample rate (1 = every match)</span>
              </div>
              <div className="row">
                <input style={{ width: 110 }} type="number" step="0.05" min="0" max="1"
                  title="Alert when a score is below this; 0 disables alerts"
                  value={form.alert_threshold}
                  onChange={(e) => setForm({ ...form, alert_threshold: e.target.value })} />
                <span style={{ color: "var(--muted)", fontSize: 12 }}>alert below (0 = no alerts)</span>
              </div>
            </div>
            <div style={{ display: "grid", gap: 8 }}>
              <div className="row">
                <select value={form.filter_status} onChange={(e) => setForm({ ...form, filter_status: e.target.value })}>
                  <option value="">Any status</option>
                  <option value="ok">status = ok</option>
                  <option value="error">status = error</option>
                </select>
                <input placeholder="model (exact)" value={form.filter_model}
                  onChange={(e) => setForm({ ...form, filter_model: e.target.value })} />
              </div>
              <input placeholder="trace name contains…" value={form.filter_name}
                onChange={(e) => setForm({ ...form, filter_name: e.target.value })} />
              <div style={{ color: "var(--muted)", fontSize: 11 }}>SCORERS</div>
              <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
                {scorers.map((s) => (
                  <label key={s.name} className="row" style={{ fontSize: 12, color: "var(--muted)", gap: 4 }}>
                    <input type="checkbox" checked={form.scorers.includes(s.name)} onChange={() => toggleScorer(s.name)} />
                    {s.name}
                  </label>
                ))}
              </div>
            </div>
          </div>
          <div className="row" style={{ marginTop: 10 }}>
            <button className="small" onClick={create} disabled={!form.name.trim() || !form.scorers.length}>
              Create rule
            </button>
          </div>
        </div>
      )}

      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr><th>Rule</th><th>Matches</th><th>Scorers</th><th>Sample</th><th>Alert below</th><th>State</th><th /></tr>
          </thead>
          <tbody>
            {rules.map((r) => (
              <tr key={r.id}>
                <td style={{ fontWeight: 600 }}>{r.name}</td>
                <td className="mono" style={{ color: "var(--muted)", fontSize: 11 }}>{filterSummary(r)}</td>
                <td className="mono" style={{ fontSize: 11 }}>{(r.scorers || []).join(", ")}</td>
                <td>{Math.round((r.sample_rate || 0) * 100)}%</td>
                <td>{r.alert_threshold == null ? "–" : Number(r.alert_threshold).toFixed(2)}</td>
                <td><span className={"pill " + (r.enabled ? "ok" : "")}>{r.enabled ? "enabled" : "paused"}</span></td>
                <td>
                  <div className="row" style={{ justifyContent: "flex-end" }}>
                    <button className="small ghost" onClick={() => toggleEnabled(r)}>{r.enabled ? "Pause" : "Resume"}</button>
                    <button className="small ghost" onClick={() => remove(r)}>Delete</button>
                  </div>
                </td>
              </tr>
            ))}
            {!rules.length && (
              <tr><td colSpan={7} className="empty">No rules yet. Create one to score live traffic automatically.</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="row" style={{ marginTop: 24, justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>Open alerts</h2>
        <Link className="btn ghost" to={`/projects/${pid}/traces`}>Open logs →</Link>
      </div>
      <p className="sub" style={{ marginTop: 4 }}>Scores below a rule's threshold, newest first.</p>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Rule</th><th>Trace</th><th>Scorer</th><th>Score</th><th>Threshold</th><th>When</th><th /></tr></thead>
          <tbody>
            {alerts.map((a) => (
              <tr key={a.id}>
                <td style={{ fontWeight: 600 }}>{a.rule_name || a.rule_id}</td>
                <td>
                  <Link className="mono" style={{ fontSize: 11 }} to={`/projects/${pid}/traces/${a.trace_id}`}>
                    {String(a.trace_id).length > 24 ? String(a.trace_id).slice(0, 24) + "…" : a.trace_id}
                  </Link>
                </td>
                <td className="mono" style={{ fontSize: 11 }}>{a.scorer}</td>
                <td className="neg">{a.score == null ? "–" : Number(a.score).toFixed(2)}</td>
                <td>{Number(a.threshold).toFixed(2)}</td>
                <td style={{ color: "var(--muted)", fontSize: 11 }}>{new Date(a.created_at).toLocaleString()}</td>
                <td><button className="small ghost" onClick={() => ack(a)}>Acknowledge</button></td>
              </tr>
            ))}
            {!alerts.length && <tr><td colSpan={7} className="empty">No open alerts.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
