"""Seed the Go server with realistic demo data.

The Go server owns the database; this script only talks to its HTTP API, so
seeding is a pure client concern (same contract an external tool would use).

Usage:  python worker-py/seed.py [--base http://localhost:8000]
"""
import argparse, json, random, sys, time, urllib.request

KB = [
    ("reset password", "Use the account recovery flow: click 'Forgot password' and follow the email link."),
    ("refund status", "Refunds post within 5-7 business days to the original payment method."),
    ("change plan", "You can upgrade in Settings > Billing > Change plan; proration is automatic."),
    ("api 401 error", "A 401 means the API key is invalid or revoked. Regenerate it in Settings > API keys."),
    ("cancel subscription", "Cancel anytime in Settings > Billing > Cancel; access lasts until period end."),
]

# Query intent -> KB topic. The demo only makes sense when each question is
# answered with the matching document, so every phrasing is mapped explicitly
# instead of falling back to a random topic.
ALIASES = {
    "reset password": ["reset my password", "reset password", "forgot password"],
    "refund status": ["refund status", "refund yet"],
    "change plan": ["change my plan", "change plan"],
    "api 401 error": ["401"],
    "cancel subscription": ["cancel my subscription", "cancel sub"],
}

MODELS = ["gpt-4o-mini", "gpt-4o-mini", "gpt-4o", "claude-sonnet-4"]

BASE = "http://localhost:8000"
_seq = 0


def new_id():
    global _seq
    _seq += 1
    return f"{time.time_ns()}-{_seq}"


def call(method, path, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        BASE + path, data=data, method=method,
        headers={"Content-Type": "application/json"} if data else {},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        body = r.read().decode()
        return json.loads(body) if body else {}


# Demo conversations. Each list is one thread; the turns are ingested as
# separate traces with a shared thread_id so the Conversations UI has realistic
# multi-turn data instead of only single-request threads.
CONVERSATIONS = [
    ["How do I reset my password?",
     "I clicked the link but it expired. What now?",
     "Can I reset it from the mobile app too?"],
    ["Where do I check my refund status?",
     "It has been 8 days and the refund still is not showing.",
     "Who do I contact if it never arrives?"],
    ["How can I change my plan?",
     "Will I be charged the full amount or prorated?",
     "Can I switch back to monthly later?"],
    ["I keep getting a 401 from the API",
     "I regenerated the key and it still fails.",
     "Does deleting the old key take effect immediately?"],
    ["How do I cancel my subscription?",
     "Will I keep access until the end of the billing period?",
     "Can I export my data before it closes?"],
    ["Reset password steps please",
     "The email never arrived. I checked spam already.",
     "Is there a phone verification fallback?"],
    ["my api returns 401 always why",
     "The key works in curl but not in the SDK.",
     "Which header should the SDK send?"],
    ["change plan to annual",
     "Does the annual plan include priority support?",
     "Can I add more seats later?"],
]

SINGLES = [
    "How do I reset my password?", "Where do I check my refund status?",
    "How can I change my plan?", "I keep getting a 401 from the API",
    "How do I cancel my subscription?", "refund yet not arrived",
    "401 on every call", "can u cancel sub for me",
    "change plan to annual", "I forgot password what now",
    "my api returns 401 always why", "reset password steps please",
]


def ingest_turn(pid, q, trace_start, thread_id=None):
    """Ingest one trace (one turn) and optionally attach it to a conversation."""
    model = random.choice(MODELS)
    root_id = new_id()
    kb_q = next((k for k in KB if any(a in q.lower() for a in ALIASES[k[0]])), None) or random.choice(KB)
    retrieved = f"doc:{kb_q[0].replace(' ', '_')} :: {kb_q[1]}"

    events = [{
        "id": root_id, "trace_id": root_id, "name": "support_agent", "type": "task",
        "input": q, "start_ms": trace_start,
        "metadata": {"environment": "production", "user_id": f"u{random.randint(1, 40)}",
                     "channel": random.choice(["web", "api", "slack"])},
    }]
    t_retr = trace_start + 30
    retr_span = {
        "id": new_id(), "trace_id": root_id, "parent_id": root_id,
        "name": "retrieve_docs", "type": "tool",
        "input": {"query": q},
        "output": {"query": q, "doc_id": kb_q[0].replace(" ", "_"), "snippet": kb_q[1]},
        "start_ms": t_retr, "end_ms": t_retr + random.randint(60, 400),
        "metadata": {"index": "kb_v2"},
    }
    if random.random() < 0.05:
        retr_span["error"] = {"message": "vector index timeout", "type": "tool_error"}
        retr_span["output"] = None
    events.append(retr_span)
    ok = not retr_span.get("error")

    llm_span = {
        "id": new_id(), "trace_id": root_id, "parent_id": root_id,
        "name": "generate_answer", "type": "llm", "model": model,
        "input": [retrieved, q],
        "start_ms": retr_span["end_ms"], "end_ms": retr_span["end_ms"] + random.randint(500, 3500),
    }
    if ok:
        h = sum(ord(c) for c in q) % 10
        if h < 5:
            ans = kb_q[1]
        elif h < 7:
            ans = f"I believe you can find this in your account settings. {kb_q[1]}"
        else:
            ans = "Sorry, I could not find relevant information about your request."
        if model == "claude-sonnet-4":
            ans += f" (doc ref: {kb_q[0].replace(' ', '_')})"
        llm_span["output"] = ans
    else:
        llm_span["error"] = {"message": "provider rate limit exceeded", "type": "provider_error"}
    events.append(llm_span)

    if ok and random.random() < 0.3:
        events.append({
            "id": new_id(), "trace_id": root_id, "parent_id": root_id,
            "name": "verify_tone", "type": "score",
            "input": llm_span.get("output"),
            "output": {"tone_pass": True, "score": round(random.uniform(0.7, 1.0), 2)},
            "start_ms": llm_span["end_ms"], "end_ms": llm_span["end_ms"] + random.randint(20, 120),
        })
    if ok:
        events[0]["output"] = llm_span.get("output")

    payload = {"events": events}
    if thread_id:
        payload["thread_id"] = thread_id
    call("POST", f"/api/v1/projects/{pid}/spans:ingest", payload)

    if random.random() < 0.35 and ok:
        good = random.random() < 0.7
        call("POST", f"/api/v1/projects/{pid}/traces/{root_id}/feedback", {
            "kind": "thumbs", "value": "up" if good else "down",
            "comment": None if good else random.choice(
                ["missed the retrieval", "answer too vague", "wrong doc cited"]),
        })
    return root_id, ok


def seed():
    random.seed(42)
    projects = call("GET", "/api/v1/projects")
    existing = next((p for p in projects if p["name"] == "Demo Support Agent"), None)
    conversations_only = False
    if existing:
        pid = existing["id"]
        threads = call("GET", f"/api/v1/projects/{pid}/threads?limit=100").get("threads", [])
        if any(int(t.get("turn_count") or 0) > 1 for t in threads):
            print("project already exists with multi-turn conversations - nothing to seed")
            return 0
        conversations_only = True
        print("project exists; adding multi-turn conversations to", pid)
    else:
        pid = call("POST", "/api/v1/projects", {
            "name": "Demo Support Agent",
            "description": "Support bot: retrieve KB answer, grade response",
        })["id"]
        print("created project", pid)

    if not conversations_only:
        for name, model, template, params in [
            ("support-answer-v1", "gpt-4o-mini", {"messages": [
                {"role": "system", "content": "You are a helpful support agent. Answer concisely using the retrieved context."},
                {"role": "user", "content": "Retrieved context:\n{{input}}\n\nAnswer the customer question."},
            ]}, {"temperature": 0.2}),
            ("support-answer-v2", "gpt-4o", {"messages": [
                {"role": "system", "content": "You are a meticulous support agent. Cite the doc section id in your answer."},
                {"role": "user", "content": "Retrieved context:\n{{input}}\n\nAnswer the customer question with a doc reference."},
            ]}, {"temperature": 0.1}),
        ]:
            call("POST", f"/api/v1/projects/{pid}/prompts",
                 {"name": name, "model": model, "template": template, "params": params})

    # Spread the work over ~6 days so the dashboard timeseries (daily buckets
    # over the default 7-day window) shows a real curve, and every seeded trace
    # still falls inside that window. Multi-turn conversations keep their turns
    # a couple of minutes apart inside one 4-hour slot.
    base = int(time.time() * 1000) - 6 * 24 * 3600 * 1000
    step = 4 * 3600 * 1000
    units = [("conversation", c) for c in CONVERSATIONS] + [("single", [q]) for q in SINGLES]
    random.shuffle(units)
    conversations = []
    for ui, (kind, messages) in enumerate(units):
        slot = base + ui * step + random.randint(-600_000, 600_000)
        thread_id = f"conv-{ui + 1:02d}" if kind == "conversation" else None
        if thread_id:
            conversations.append(thread_id)
        for ti, q in enumerate(messages):
            ingest_turn(pid, q, slot + ti * random.randint(60_000, 180_000), thread_id)

    # Seed a few manual conversation scores so the dashboard and the
    # conversation page have data before anyone clicks "Evaluate".
    threads = call("GET", f"/api/v1/projects/{pid}/threads?limit=100").get("threads", [])
    seeded_threads = [t for t in threads if str(t.get("id", "")).startswith("conv-")]
    for t in seeded_threads[:6]:
        for name, lo, hi in [
            ("thread_completion", 0.80, 1.00),
            ("thread_error_free", 0.70, 1.00),
            ("thread_turn_consistency", 0.55, 0.95),
        ]:
            call("POST", f"/api/v1/projects/{pid}/threads/{t['id']}/scores", {
                "name": name, "score": round(random.uniform(lo, hi), 2),
                "rationale": "seeded review", "source": "manual",
            })

    # Run one real conversation evaluation through the Go queue + Python worker,
    # so the demo shows worker-generated scores and evaluation history too.
    if conversations:
        target = conversations[0]
        try:
            job = call("POST", f"/api/v1/projects/{pid}/threads/{target}/evaluate", {
                "scorers": ["thread_completion", "thread_error_free", "thread_turn_consistency"],
            })
            deadline = time.time() + 45
            while time.time() < deadline:
                detail = call("GET", f"/api/v1/projects/{pid}/threads/{target}")
                row = next((e for e in detail.get("evaluations", [])
                            if e.get("job_id") == job.get("job_id")), None)
                if row and row.get("status") in ("done", "failed"):
                    break
                time.sleep(0.5)
        except Exception as e:
            print("thread evaluation seed skipped:", e)

    if not conversations_only:
        ds = call("POST", f"/api/v1/projects/{pid}/datasets", {
            "name": "Support regression cases", "description": "Cases captured from production review"})["id"]
        call("POST", f"/api/v1/datasets/{ds}/records", [
            {"input": "How do I reset my password?", "expected": KB[0][1], "metadata": {"category": "auth"}, "tags": ["regression"]},
            {"input": "Where do I check my refund status?", "expected": KB[1][1], "metadata": {"category": "billing"}, "tags": ["regression"]},
            {"input": "How can I change my plan?", "expected": KB[2][1], "metadata": {"category": "billing"}, "tags": ["regression"]},
            {"input": "I keep getting a 401 from the API", "expected": KB[3][1], "metadata": {"category": "api"}, "tags": ["regression"]},
            {"input": "How do I cancel my subscription?", "expected": KB[4][1], "metadata": {"category": "billing"}, "tags": ["regression"]},
        ])

        ds2 = call("POST", f"/api/v1/projects/{pid}/datasets", {
            "name": "API error FAQ", "description": "Upload from the CSV import"})["id"]
        call("POST", f"/api/v1/datasets/{ds2}/records", [
            {"input": "What does 401 mean on the API?", "expected": "/reset the key | invalid or revoked", "metadata": {"category": "api"}},
            {"input": "Can I use two API keys?", "expected": "Yes, create multiple keys in Settings > API keys with distinct scopes.", "metadata": {"category": "api"}},
        ])

    catalog = "traces and conversations" if conversations_only else "traces, 2 datasets, 2 prompts"
    print(f"seeded {len(CONVERSATIONS) * 3 + len(SINGLES)} traces "
          f"({len(CONVERSATIONS)} conversations) and {catalog} into project {pid}")
    return 0


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://localhost:8000")
    args = ap.parse_args()
    BASE = args.base
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass
    sys.exit(seed())
