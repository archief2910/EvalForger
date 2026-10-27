import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { api } from "../api.js";

// The optimization loop: evaluate, propose a better prompt, re-evaluate, keep
// the winner. A run is pinned to a dataset snapshot and a baseline prompt, and
// the winning prompt is only promoted when it actually scored higher.

const EMPTY = {
  name: "", dataset_id: "", prompt_version_id: "", scorers: ["exact_match"], max_candidates: 4,
};

function pct(v) {
  return v == null ? "—" : Number(v).toFixed(3);
}

export default function Optimize() {
  const { pid } = useParams();
  const [runs, setRuns] = useState([]);
  const [datasets, setDatasets] = useState([]);
  const [prompts, setPrompts] = useState([]);
  const [scorers, setScorers] = useState([]);
  const [form, setForm] = useState(EMPTY);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const [open, setOpen] = useState(null);

  const load = () => {
    api.get(`/api/v1/projects/${pid}/optimizations`).then(setRuns).catch(() => setRuns([]));
  };

  useEffect(() => {
    load();
    api.get(`/api/v1/projects/${pid}/datasets`).then(setDatasets).catch(() => setDatasets([]));
    api.get(`/api/v1/projects/${pid}/prompts`).then(setPrompts).catch(() => setPrompts([]));
    api.get(`/api/v1/scorers?scope=record`).then(setScorers).catch(() => setScorers([]));
    // A run is long-lived; poll while the page is open so status changes show.
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
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
      await api.post(`/api/v1/projects/${pid}/optimizations`, {
        ...form,
        max_candidates: Number(form.max_candidates),
      });
      setCreating(false);
      setForm(EMPTY);
      load();
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const detail = async (run) => {
    if (open === run.id) {
      setOpen(null);
      return;
    }
    try {
      const full = await api.get(`/api/v1/projects/${pid}/optimizations/${run.id}`);
      setOpen(run.id);
      setRuns((rs) => rs.map((r) => (r.id === run.id ? { ...r, ...full } : r)));
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const latestPromptByDataset = (datasetId) =>
    prompts.filter((p) => p.id).length ? prompts : [];

  return (
    <div>
      <h1>Optimize</h1>
      <p className="sub">
        The half of the loop that changes something. A run evaluates the baseline and a set of prompt
        candidates on the same pinned dataset with the same scorers, then keeps the best one — but only
        if it beat the baseline. Ties and regressions are reported and discarded.
      </p>

      <div className="toolbar">
        <button className="small" onClick={() => setCreating((v) => !v)}>
          {creating ? "Cancel" : "+ New optimization"}
        </button>
        <div className="spacer" />
        <span style={{ color: "var(--muted)" }}>{runs.length} run{runs.length === 1 ? "" : "s"}</span>
      </div>
      {error && <div className="panel" style={{ color: "var(--red)", fontSize: 12 }}>{error}</div>}

      {creating && (
        <div className="panel" style={{ marginBottom: 14 }}>
          <b>New optimization</b>
          <div className="grid cols2" style={{ marginTop: 10 }}>
            <div style={{ display: "grid", gap: 8 }}>
              <input placeholder="Run name" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} />
              <select value={form.dataset_id}
                onChange={(e) => setForm({ ...form, dataset_id: e.target.value })}>
                <option value="">Select a dataset…</option>
                {datasets.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}
              </select>
              <select value={form.prompt_version_id}
                onChange={(e) => setForm({ ...form, prompt_version_id: e.target.value })}>
                <option value="">No baseline prompt (use the built-in one)</option>
                {latestPromptByDataset(form.dataset_id).map((p) => (
                  <option key={p.id} value={p.id}>{p.name} v{p.version}</option>
                ))}
              </select>
              <div className="row">
                <input style={{ width: 90 }} type="number" min="1" max="8" value={form.max_candidates}
                  onChange={(e) => setForm({ ...form, max_candidates: e.target.value })} />
                <span style={{ color: "var(--muted)", fontSize: 12 }}>candidates to try (1–8)</span>
              </div>
            </div>
            <div style={{ display: "grid", gap: 8, alignContent: "start" }}>
              <div style={{ color: "var(--muted)", fontSize: 11 }}>OBJECTIVE SCORERS</div>
              <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
                {scorers.map((s) => (
                  <label key={s.name} className="row" style={{ fontSize: 12, color: "var(--muted)", gap: 4 }}>
                    <input type="checkbox" checked={form.scorers.includes(s.name)}
                      onChange={() => toggleScorer(s.name)} />
                    {s.name}
                  </label>
                ))}
              </div>
              <div style={{ color: "var(--muted)", fontSize: 11, lineHeight: 1.5 }}>
                The objective is the mean score across records and scorers. Every candidate is measured
                on the same dataset snapshot, so the comparison is fair.
              </div>
            </div>
          </div>
          <div className="row" style={{ marginTop: 10 }}>
            <button className="small" onClick={create}
              disabled={!form.dataset_id || !form.scorers.length}>Start optimization</button>
          </div>
        </div>
      )}

      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr><th>Run</th><th>Status</th><th>Baseline</th><th>Best</th><th>Gain</th><th>Kept</th><th /></tr>
          </thead>
          <tbody>
            {runs.map((r) => {
              const gain = (r.best_score != null && r.baseline_score != null)
                ? r.best_score - r.baseline_score : null;
              return (
                <tr key={r.id}>
                  <td style={{ fontWeight: 600 }}>{r.name}</td>
                  <td>
                    <span className={"pill " + (r.status === "completed" ? "ok" : r.status === "failed" ? "bad" : "")}>
                      {r.status}
                    </span>
                  </td>
                  <td>{pct(r.baseline_score)}</td>
                  <td>{pct(r.best_score)}</td>
                  <td className={gain == null ? "" : gain > 0 ? "pos" : "neg"}>
                    {gain == null ? "—" : (gain > 0 ? "+" : "") + gain.toFixed(3)}
                  </td>
                  <td style={{ fontSize: 12 }}>
                    {r.best_prompt_version_id ? "promoted" : r.status === "completed" ? "no improvement" : "—"}
                  </td>
                  <td>
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button className="small ghost" onClick={() => detail(r)}>
                        {open === r.id ? "Hide" : "Details"}
                      </button>
                    </div>
                  </td>
                </tr>
              );
            })}
            {!runs.length && (
              <tr><td colSpan={7} className="empty">
                No runs yet. Create one to search for a better prompt.
              </td></tr>
            )}
          </tbody>
        </table>
      </div>

      {runs.filter((r) => r.id === open).map((r) => (
        <div key={r.id} className="panel" style={{ marginTop: 12 }}>
          <b>{r.name}</b>
          {r.error && <div style={{ color: "var(--red)", fontSize: 12, marginTop: 6 }}>{r.error}</div>}
          {r.summary?.candidates && (
            <div style={{ marginTop: 10 }}>
              <div style={{ color: "var(--muted)", fontSize: 11 }}>CANDIDATES</div>
              <table style={{ marginTop: 6 }}>
                <thead><tr><th>Strategy</th><th>Mean</th><th>Per scorer</th></tr></thead>
                <tbody>
                  {(r.summary.candidates || []).map((c) => (
                    <tr key={c.name}>
                      <td className="mono" style={{ fontSize: 12 }}>{c.name}</td>
                      <td>{pct(c.mean)}</td>
                      <td className="mono" style={{ fontSize: 11, color: "var(--muted)" }}>
                        {Object.entries(c.scores || {}).map(([k, v]) => `${k}=${pct(v)}`).join(", ")}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {r.best_prompt && (
            <div style={{ marginTop: 10 }}>
              <div style={{ color: "var(--muted)", fontSize: 11 }}>
                WINNING PROMPT (v{r.best_prompt.version})
              </div>
              <pre className="mono" style={{ fontSize: 11, whiteSpace: "pre-wrap", marginTop: 4 }}>
                {typeof r.best_prompt.template === "string"
                  ? r.best_prompt.template
                  : JSON.stringify(r.best_prompt.template, null, 2)}
              </pre>
            </div>
          )}
          {!r.summary && r.status !== "completed" && r.status !== "failed" && (
            <div style={{ color: "var(--muted)", fontSize: 12, marginTop: 6 }}>
              Waiting for the worker. This page refreshes every 5 seconds.
            </div>
          )}
        </div>
      ))}
    </div>
  );
}