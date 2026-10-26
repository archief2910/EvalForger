import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, fmt } from "../api.js";

// A conversation is a group of traces ("turns") that share a thread id. The
// list is intentionally conversation-first: one row per conversation, with the
// turn count and conversation-level score, not one row per request.
export default function Conversations() {
  const { pid } = useParams();
  const nav = useNavigate();
  const [data, setData] = useState({ threads: [], total: 0 });
  const [flt, setFlt] = useState({ status: "", search: "", sort_by: "last_activity_ms" });

  const load = (f) => {
    const q = new URLSearchParams({ limit: 100 });
    for (const [k, v] of Object.entries(f || flt)) if (v) q.set(k, v);
    api.get(`/api/v1/projects/${pid}/threads?${q}`).then(setData).catch(() => {});
  };
  useEffect(() => { load(); }, [pid]);

  const set = (patch) => {
    const nf = { ...flt, ...patch };
    setFlt(nf);
    load(nf);
  };

  return (
    <div>
      <h1>Conversations</h1>
      <p className="sub">
        {fmt.num(data.total)} conversations. Each conversation groups its traces as turns and can be
        scored as a whole, so multi-turn quality is visible instead of judging every turn alone.
      </p>
      <div className="toolbar">
        <input placeholder="Search turns…" style={{ width: 220 }} value={flt.search}
          onChange={(e) => set({ search: e.target.value })} />
        <select value={flt.status} onChange={(e) => set({ status: e.target.value })}>
          <option value="">All statuses</option>
          <option value="open">open</option>
          <option value="closed">closed</option>
        </select>
        <div className="spacer" />
        <span style={{ color: "var(--muted)" }}>Sort</span>
        <select value={flt.sort_by} onChange={(e) => set({ sort_by: e.target.value })}>
          <option value="last_activity_ms">last activity</option>
          <option value="started_at_ms">started</option>
          <option value="trace_count">turns</option>
          <option value="avg_score">avg score</option>
        </select>
        <Link className="btn ghost" to={`/projects/${pid}/traces`}>All traces →</Link>
      </div>
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Conversation</th><th>Status</th><th>Turns</th><th>Duration</th>
              <th>Avg score</th><th>Scores</th><th>Last activity</th>
            </tr>
          </thead>
          <tbody>
            {data.threads.map((t) => (
              <tr key={t.id} className="clickable" onClick={() => nav(`/projects/${pid}/conversations/${t.id}`)}>
                <td style={{ maxWidth: 380 }}>
                  <div className="mono" style={{ fontSize: 12 }}>{t.id.length > 32 ? t.id.slice(0, 32) + "…" : t.id}</div>
                  <div style={{ color: "var(--muted)", fontSize: 12, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
                    {t.last_input == null ? "—" : typeof t.last_input === "string" ? t.last_input : JSON.stringify(t.last_input)}
                  </div>
                </td>
                <td><span className={"pill " + (t.status === "open" ? "ok" : "")}>{t.status}</span></td>
                <td>{fmt.num(t.turn_count)}</td>
                <td>{fmt.ms(t.duration_ms)}</td>
                <td>
                  {t.avg_score == null ? <span className="zero">–</span> : (
                    <span className={t.avg_score >= 0.8 ? "pos" : t.avg_score < 0.5 ? "neg" : ""}>
                      {Number(t.avg_score).toFixed(2)}
                    </span>
                  )}
                </td>
                <td style={{ color: "var(--muted)" }}>{fmt.num(t.score_count)}</td>
                <td style={{ color: "var(--muted)", fontSize: 12 }}>
                  {new Date(t.last_activity_ms).toLocaleString()}
                </td>
              </tr>
            ))}
            {!data.threads.length && (
              <tr><td colSpan={7} className="empty">No conversations match the filters.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
