import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { api } from "../api.js";

// Monitors watch the fleet, not one request. This page is the operational
// surface: aggregate conditions over a window, how long telemetry is kept, and
// the user-authored metrics that can be scored alongside the builtins.

const METRICS = [
  ["error_rate", "error rate (share of traces)"],
  ["avg_score", "average score"],
  ["p95_latency_ms", "p95 latency (ms)"],
  ["avg_latency_ms", "average latency (ms)"],
  ["avg_cost_usd", "average cost per trace (USD)"],
  ["span_count", "span count"],
  ["trace_count", "trace count"],
  ["error_count", "errored trace count"],
];

const EMPTY_MONITOR = {
  name: "", metric: "error_rate", operator: ">", threshold: 0.05,
  window_seconds: 900, channel_type: "none", channel_url: "", channel_secret: "",
};

const EMPTY_METRIC = { name: "", description: "", code: "def score(payload):\n    return 0.5\n" };

const EMPTY_RETENTION = { name: "", retention_days: 30, apply_to_past: true };

const EMPTY_GUARDRAIL = {
  name: "", mode: "flag", checkType: "PII", topics: "", keywords: "", regexes: "",
};

function statePill(state) {
  if (state === "firing") return <span className="pill bad">firing</span>;
  return <span className="pill ok">ok</span>;
}

export default function Monitors() {
  const { pid } = useParams();
  const [monitors, setMonitors] = useState([]);
  const [events, setEvents] = useState([]);
  const [retention, setRetention] = useState([]);
  const [metrics, setMetrics] = useState([]);
  const [sandbox, setSandbox] = useState(true);
  const [policies, setPolicies] = useState([]);
  const [guardEvents, setGuardEvents] = useState([]);
  const [guardForm, setGuardForm] = useState(EMPTY_GUARDRAIL);
  const [guardOpen, setGuardOpen] = useState(false);
  const [guardError, setGuardError] = useState("");
  const [testText, setTestText] = useState("email me at jane@example.com");
  const [testOut, setTestOut] = useState(null);
  const [form, setForm] = useState(EMPTY_MONITOR);
  const [metricForm, setMetricForm] = useState(EMPTY_METRIC);
  const [retentionForm, setRetentionForm] = useState(EMPTY_RETENTION);
  const [creating, setCreating] = useState(false);
  const [registering, setRegistering] = useState(false);
  const [error, setError] = useState("");
  const [metricError, setMetricError] = useState("");
  const [testResult, setTestResult] = useState(null);

  const load = () => {
    api.get(`/api/v1/projects/${pid}/monitors`).then(setMonitors).catch(() => setMonitors([]));
    api.get(`/api/v1/projects/${pid}/monitor-events`).then((d) => setEvents(d.events || [])).catch(() => setEvents([]));
    api.get(`/api/v1/projects/${pid}/retention-rules`).then(setRetention).catch(() => setRetention([]));
    api.get(`/api/v1/projects/${pid}/custom-scorers`).then((d) => {
      setMetrics(d.scorers || []);
      setSandbox(d.sandbox_available !== false);
    }).catch(() => setMetrics([]));
    api.get(`/api/v1/projects/${pid}/guardrail-policies`).then(setPolicies).catch(() => setPolicies([]));
    api.get(`/api/v1/projects/${pid}/guardrail-events`).then((d) => setGuardEvents(d.events || [])).catch(() => setGuardEvents([]));
  };
  useEffect(() => { load(); }, [pid]);

  const createMonitor = async () => {
    setError("");
    try {
      await api.post(`/api/v1/projects/${pid}/monitors`, {
        ...form,
        threshold: Number(form.threshold),
        window_seconds: Number(form.window_seconds),
      });
      setForm(EMPTY_MONITOR);
      setCreating(false);
      load();
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const evaluate = async (m) => {
    setError("");
    try {
      await api.post(`/api/v1/projects/${pid}/monitors/${m.id}/evaluate`);
      load();
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const toggleMonitor = async (m) => {
    await api.patch(`/api/v1/projects/${pid}/monitors/${m.id}`, { enabled: !m.enabled });
    load();
  };

  const removeMonitor = async (m) => {
    await api.del(`/api/v1/projects/${pid}/monitors/${m.id}`);
    load();
  };

  const createRetention = async () => {
    setError("");
    try {
      await api.post(`/api/v1/projects/${pid}/retention-rules`, {
        ...retentionForm, retention_days: Number(retentionForm.retention_days),
      });
      setRetentionForm(EMPTY_RETENTION);
      load();
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const applyRetention = async (rule) => {
    setError("");
    try {
      const out = await api.post(`/api/v1/projects/${pid}/retention-rules/${rule.id}/apply`);
      const a = out.applied || {};
      setError(`Expired ${a.spans_deleted || 0} spans, ${a.feedback_deleted || 0} feedback rows, ${a.threads_deleted || 0} conversations.`);
      load();
    } catch (e) {
      setError(String(e.message || e));
    }
  };

  const removeRetention = async (rule) => {
    await api.del(`/api/v1/projects/${pid}/retention-rules/${rule.id}`);
    load();
  };

  const registerMetric = async () => {
    setMetricError("");
    try {
      await api.post(`/api/v1/projects/${pid}/custom-scorers`, metricForm);
      setMetricForm(EMPTY_METRIC);
      setRegistering(false);
      load();
    } catch (e) {
      setMetricError(String(e.message || e));
    }
  };

  const testMetric = async (name) => {
    setMetricError("");
    setTestResult(null);
    try {
      const out = await api.post(`/api/v1/projects/${pid}/custom-scorers/${name}/test`);
      setTestResult(out);
    } catch (e) {
      setMetricError(String(e.message || e));
    }
  };

  const removeMetric = async (name) => {
    await api.del(`/api/v1/projects/${pid}/custom-scorers/${name}`);
    load();
  };

  const splitList = (v) => v.split(",").map((s) => s.trim()).filter(Boolean);

  const createGuardrail = async () => {
    setGuardError("");
    const config = {};
    if (guardForm.checkType === "TOPIC") config.topics = splitList(guardForm.topics);
    if (guardForm.checkType === "CUSTOM") {
      config.keywords = splitList(guardForm.keywords);
      config.regexes = splitList(guardForm.regexes);
    }
    try {
      await api.post(`/api/v1/projects/${pid}/guardrail-policies`, {
        name: guardForm.name, mode: guardForm.mode,
        checks: [{ type: guardForm.checkType, config }],
      });
      setGuardForm(EMPTY_GUARDRAIL);
      setGuardOpen(false);
      load();
    } catch (e) {
      setGuardError(String(e.message || e));
    }
  };

  const removeGuardrail = async (p) => {
    await api.del(`/api/v1/projects/${pid}/guardrail-policies/${p.id}`);
    load();
  };

  const runGuardTest = async () => {
    setGuardError("");
    try {
      setTestOut(await api.post(`/api/v1/projects/${pid}/guardrails/check`, { text: testText }));
      load();
    } catch (e) {
      setGuardError(String(e.message || e));
    }
  };

  return (
    <div>
      <h1>Monitors &amp; policies</h1>
      <p className="sub">
        Online rules score individual traces. A monitor watches an aggregate over a window and fires once
        when it crosses a threshold, resolving when it recovers. Retention and custom metrics live here too.
      </p>
      {error && <div className="panel" style={{ color: "var(--muted)", fontSize: 12 }}>{error}</div>}

      <div className="toolbar" style={{ marginTop: 16 }}>
        <h2 style={{ margin: 0 }}>Monitors</h2>
        <div className="spacer" />
        <button className="small" onClick={() => setCreating((v) => !v)}>
          {creating ? "Cancel" : "+ New monitor"}
        </button>
      </div>

      {creating && (
        <div className="panel" style={{ marginBottom: 14 }}>
          <b>New monitor</b>
          <div className="grid cols2" style={{ marginTop: 10 }}>
            <div style={{ display: "grid", gap: 8 }}>
              <input placeholder="Name (e.g. error rate watch)" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} />
              <select value={form.metric} onChange={(e) => setForm({ ...form, metric: e.target.value })}>
                {METRICS.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
              </select>
              <div className="row">
                <select style={{ width: 90 }} value={form.operator}
                  onChange={(e) => setForm({ ...form, operator: e.target.value })}>
                  <option value="&gt;">above</option>
                  <option value="&lt;">below</option>
                </select>
                <input style={{ width: 110 }} type="number" step="any" value={form.threshold}
                  onChange={(e) => setForm({ ...form, threshold: e.target.value })} />
                <input style={{ width: 120 }} type="number" min="60" value={form.window_seconds}
                  onChange={(e) => setForm({ ...form, window_seconds: e.target.value })} />
                <span style={{ color: "var(--muted)", fontSize: 12 }}>seconds</span>
              </div>
            </div>
            <div style={{ display: "grid", gap: 8 }}>
              <select value={form.channel_type} onChange={(e) => setForm({ ...form, channel_type: e.target.value })}>
                <option value="none">No notification</option>
                <option value="webhook">Webhook (JSON)</option>
                <option value="slack">Slack webhook</option>
              </select>
              {form.channel_type !== "none" && (
                <>
                  <input placeholder="https://hooks.slack.com/..." value={form.channel_url}
                    onChange={(e) => setForm({ ...form, channel_url: e.target.value })} />
                  <input placeholder="Signing secret (optional)" value={form.channel_secret}
                    onChange={(e) => setForm({ ...form, channel_secret: e.target.value })} />
                </>
              )}
            </div>
          </div>
          <div className="row" style={{ marginTop: 10 }}>
            <button className="small" onClick={createMonitor} disabled={!form.name.trim()}>Create monitor</button>
          </div>
        </div>
      )}

      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Monitor</th><th>Condition</th><th>Window</th><th>Last value</th><th>State</th><th /></tr></thead>
          <tbody>
            {monitors.map((m) => (
              <tr key={m.id}>
                <td style={{ fontWeight: 600 }}>{m.name}</td>
                <td className="mono" style={{ fontSize: 11 }}>
                  {m.metric} {m.operator} {m.threshold}
                </td>
                <td>{Math.round((m.window_seconds || 0) / 60)}m</td>
                <td>{m.last_value == null ? "—" : Number(m.last_value).toFixed(4)}</td>
                <td>{m.enabled ? statePill(m.state) : <span className="pill">paused</span>}</td>
                <td>
                  <div className="row" style={{ justifyContent: "flex-end" }}>
                    <button className="small ghost" onClick={() => evaluate(m)}>Evaluate now</button>
                    <button className="small ghost" onClick={() => toggleMonitor(m)}>{m.enabled ? "Pause" : "Resume"}</button>
                    <button className="small ghost" onClick={() => removeMonitor(m)}>Delete</button>
                  </div>
                </td>
              </tr>
            ))}
            {!monitors.length && (
              <tr><td colSpan={6} className="empty">No monitors. Create one to watch an aggregate over time.</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <h2 style={{ marginTop: 26 }}>Firing history</h2>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>State</th><th>Metric</th><th>Value</th><th>Threshold</th><th>Fired</th><th>Notified</th></tr></thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.id}>
                <td><span className={"pill " + (e.state === "firing" ? "bad" : "ok")}>{e.state}</span></td>
                <td className="mono" style={{ fontSize: 11 }}>{e.metric}</td>
                <td>{e.value == null ? "—" : Number(e.value).toFixed(4)}</td>
                <td>{e.operator} {e.threshold}</td>
                <td style={{ color: "var(--muted)", fontSize: 11 }}>{new Date(e.fired_at).toLocaleString()}</td>
                <td style={{ fontSize: 11 }}>
                  {e.notified ? "delivered" : (e.notification_error ? `failed: ${e.notification_error}` : "no channel")}
                </td>
              </tr>
            ))}
            {!events.length && <tr><td colSpan={6} className="empty">Nothing has fired yet.</td></tr>}
          </tbody>
        </table>
      </div>

      <h2 style={{ marginTop: 26 }}>Retention</h2>
      <p className="sub" style={{ marginTop: 4 }}>
        How long this project keeps spans, feedback and conversations. A policy applies to new data as it ages;
        use “Apply now” to expire what is already past the window.
      </p>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Name</th><th>Keep</th><th>Past data</th><th>Enabled</th><th /></tr></thead>
          <tbody>
            {retention.map((r) => (
              <tr key={r.id}>
                <td style={{ fontWeight: 600 }}>{r.name || "policy"}</td>
                <td>{r.retention_days} days</td>
                <td>{r.apply_to_past ? "included" : "from creation only"}</td>
                <td><span className={"pill " + (r.enabled ? "ok" : "")}>{r.enabled ? "on" : "off"}</span></td>
                <td>
                  <div className="row" style={{ justifyContent: "flex-end" }}>
                    <button className="small ghost" onClick={() => applyRetention(r)}>Apply now</button>
                    <button className="small ghost" onClick={() => removeRetention(r)}>Delete</button>
                  </div>
                </td>
              </tr>
            ))}
            {!retention.length && <tr><td colSpan={5} className="empty">No policy: telemetry is kept indefinitely.</td></tr>}
          </tbody>
        </table>
      </div>
      <div className="row" style={{ marginTop: 10 }}>
        <input style={{ width: 160 }} placeholder="Policy name" value={retentionForm.name}
          onChange={(e) => setRetentionForm({ ...retentionForm, name: e.target.value })} />
        <input style={{ width: 100 }} type="number" min="1" value={retentionForm.retention_days}
          onChange={(e) => setRetentionForm({ ...retentionForm, retention_days: e.target.value })} />
        <span style={{ color: "var(--muted)", fontSize: 12 }}>days</span>
        <label className="row" style={{ fontSize: 12, color: "var(--muted)", gap: 4 }}>
          <input type="checkbox" checked={retentionForm.apply_to_past}
            onChange={(e) => setRetentionForm({ ...retentionForm, apply_to_past: e.target.checked })} />
          apply to existing data
        </label>
        <button className="small" onClick={createRetention}>Add policy</button>
      </div>

      <div className="toolbar" style={{ marginTop: 26 }}>
        <h2 style={{ margin: 0 }}>Runtime guardrails</h2>
        <div className="spacer" />
        <button className="small" onClick={() => setGuardOpen((v) => !v)}>
          {guardOpen ? "Cancel" : "+ New policy"}
        </button>
      </div>
      <p className="sub" style={{ marginTop: 4 }}>
        Protection, not evaluation. A policy runs before telemetry is stored and can be called directly
        before text is sent to a model. <b>flag</b> records the finding, <b>redact</b> masks the matched
        values, <b>block</b> refuses the request.
      </p>
      {guardError && <div className="panel" style={{ color: "var(--red)", fontSize: 12 }}>{guardError}</div>}

      {guardOpen && (
        <div className="panel" style={{ marginBottom: 14 }}>
          <b>New guardrail policy</b>
          <div className="grid cols2" style={{ marginTop: 10 }}>
            <div style={{ display: "grid", gap: 8 }}>
              <input placeholder="Policy name (e.g. no PII to providers)" value={guardForm.name}
                onChange={(e) => setGuardForm({ ...guardForm, name: e.target.value })} />
              <select value={guardForm.mode} onChange={(e) => setGuardForm({ ...guardForm, mode: e.target.value })}>
                <option value="flag">flag — record only</option>
                <option value="redact">redact — mask before storing</option>
                <option value="block">block — refuse the batch</option>
              </select>
              <select value={guardForm.checkType}
                onChange={(e) => setGuardForm({ ...guardForm, checkType: e.target.value })}>
                <option value="PII">PII — emails, phones, cards, SSNs, IPs, URLs</option>
                <option value="PROMPT_INJECTION">Prompt injection — instruction overrides</option>
                <option value="TOPIC">Topic — banned or allowed topics</option>
                <option value="CUSTOM">Custom — your keywords and regexes</option>
              </select>
            </div>
            <div style={{ display: "grid", gap: 8, alignContent: "start" }}>
              {guardForm.checkType === "TOPIC" && (
                <input placeholder="topics, comma separated (gambling, crypto)" value={guardForm.topics}
                  onChange={(e) => setGuardForm({ ...guardForm, topics: e.target.value })} />
              )}
              {guardForm.checkType === "CUSTOM" && (
                <>
                  <input placeholder="keywords, comma separated" value={guardForm.keywords}
                    onChange={(e) => setGuardForm({ ...guardForm, keywords: e.target.value })} />
                  <input placeholder="regexes, comma separated" value={guardForm.regexes}
                    onChange={(e) => setGuardForm({ ...guardForm, regexes: e.target.value })} />
                </>
              )}
              {guardForm.checkType === "PII" && (
                <div style={{ color: "var(--muted)", fontSize: 11, lineHeight: 1.5 }}>
                  Card numbers are only reported when they pass the Luhn checksum, so order numbers and
                  timestamps are not mistaken for PII.
                </div>
              )}
              <button className="small" onClick={createGuardrail} disabled={!guardForm.name.trim()}>
                Create policy
              </button>
            </div>
          </div>
        </div>
      )}

      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Policy</th><th>Mode</th><th>Checks</th><th>Enabled</th><th /></tr></thead>
          <tbody>
            {policies.map((p) => (
              <tr key={p.id}>
                <td style={{ fontWeight: 600 }}>{p.name}</td>
                <td><span className={"pill " + (p.mode === "block" ? "bad" : p.mode === "redact" ? "" : "ok")}>{p.mode}</span></td>
                <td className="mono" style={{ fontSize: 11 }}>
                  {(p.checks || []).map((c) => c.type).join(", ")}
                </td>
                <td><span className={"pill " + (p.enabled ? "ok" : "")}>{p.enabled ? "on" : "off"}</span></td>
                <td>
                  <div className="row" style={{ justifyContent: "flex-end" }}>
                    <button className="small ghost" onClick={() => removeGuardrail(p)}>Delete</button>
                  </div>
                </td>
              </tr>
            ))}
            {!policies.length && (
              <tr><td colSpan={5} className="empty">No policies: text is stored as it arrives.</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="panel" style={{ marginTop: 12 }}>
        <b>Try a check</b>
        <p className="sub" style={{ marginTop: 4 }}>
          This is the runtime endpoint an application calls before sending text to a model.
        </p>
        <div className="row">
          <input style={{ flex: 1 }} value={testText} onChange={(e) => setTestText(e.target.value)} />
          <button className="small" onClick={runGuardTest}>Run check</button>
        </div>
        {testOut && (
          <div className="mono" style={{ fontSize: 11, marginTop: 8, color: "var(--muted)" }}>
            action={testOut.action} allowed={String(testOut.allowed)}<br />
            redacted: {testOut.redacted}
          </div>
        )}
      </div>

      <h2 style={{ marginTop: 22 }}>Guardrail decisions</h2>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Action</th><th>Policy</th><th>Check</th><th>Matched</th><th>Preview</th><th>When</th></tr></thead>
          <tbody>
            {guardEvents.map((e) => (
              <tr key={e.id}>
                <td><span className={"pill " + (e.action === "block" ? "bad" : e.action === "redact" ? "" : "ok")}>{e.action}</span></td>
                <td style={{ fontWeight: 600 }}>{e.policy_name}</td>
                <td className="mono" style={{ fontSize: 11 }}>{e.check_type}</td>
                <td className="mono" style={{ fontSize: 11 }}>{e.matched}</td>
                <td style={{ color: "var(--muted)", fontSize: 11 }}>{e.preview}</td>
                <td style={{ color: "var(--muted)", fontSize: 11 }}>{new Date(e.created_at).toLocaleString()}</td>
              </tr>
            ))}
            {!guardEvents.length && (
              <tr><td colSpan={6} className="empty">Nothing has tripped yet.</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="toolbar" style={{ marginTop: 26 }}>
        <h2 style={{ margin: 0 }}>Custom metrics</h2>
        <div className="spacer" />
        <button className="small" onClick={() => setRegistering((v) => !v)}>
          {registering ? "Cancel" : "+ New metric"}
        </button>
      </div>
      <p className="sub" style={{ marginTop: 4 }}>
        User-authored Python. Metrics run only inside the sandbox — never on the host —
        and fail closed when the sandbox is unavailable.
        {!sandbox && <b style={{ color: "var(--red)" }}> Sandbox unavailable: metrics will not run.</b>}
      </p>
      {metricError && <div className="panel" style={{ color: "var(--red)", fontSize: 12 }}>{metricError}</div>}
      {testResult && (
        <div className="panel" style={{ fontSize: 12 }}>
          {testResult.ok
            ? <>Test passed: score {String(testResult.score)} {testResult.rationale ? `— ${testResult.rationale}` : ""}</>
            : <>Test failed: {testResult.error}</>}
        </div>
      )}

      {registering && (
        <div className="panel" style={{ marginBottom: 14 }}>
          <b>Register a metric</b>
          <p className="sub" style={{ marginTop: 4 }}>
            Define <code>def score(payload)</code> returning a number or <code>{"{score, rationale}"}</code>.
            For a trace, <code>payload</code> carries input/output; for a conversation, <code>turns</code>.
          </p>
          <div style={{ display: "grid", gap: 8 }}>
            <input placeholder="name (lowercase, e.g. tone_check)" value={metricForm.name}
              onChange={(e) => setMetricForm({ ...metricForm, name: e.target.value })} />
            <input placeholder="Description" value={metricForm.description}
              onChange={(e) => setMetricForm({ ...metricForm, description: e.target.value })} />
            <textarea rows={8} className="mono" style={{ fontSize: 12 }} value={metricForm.code}
              onChange={(e) => setMetricForm({ ...metricForm, code: e.target.value })} />
            <div className="row">
              <button className="small" onClick={registerMetric} disabled={!metricForm.name.trim()}>Register</button>
            </div>
          </div>
        </div>
      )}

      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Name</th><th>Description</th><th>Sandbox</th><th /></tr></thead>
          <tbody>
            {metrics.map((m) => (
              <tr key={m.id || m.name}>
                <td className="mono" style={{ fontWeight: 600 }}>{m.name}</td>
                <td style={{ color: "var(--muted)", fontSize: 12 }}>{m.description}</td>
                <td className="mono" style={{ fontSize: 11 }}>{m.sandbox || "docker"}</td>
                <td>
                  <div className="row" style={{ justifyContent: "flex-end" }}>
                    <button className="small ghost" onClick={() => testMetric(m.name)} disabled={!sandbox}>Test</button>
                    <button className="small ghost" onClick={() => removeMetric(m.name)}>Delete</button>
                  </div>
                </td>
              </tr>
            ))}
            {!metrics.length && (
              <tr><td colSpan={4} className="empty">No custom metrics. Builtin scorers work without one.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}