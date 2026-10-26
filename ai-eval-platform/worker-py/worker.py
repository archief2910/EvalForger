"""Eval worker: executed by the Go server as a subprocess per evaluation run.

Responsibilities (Python side of the Go+Python split):
  - fetch dataset records from the Go server HTTP API
  - generate task outputs: OpenRouter LLM when OPENROUTER_API_KEY is set, deterministic simulator otherwise
  - compute deterministic scorers + LLM-as-a-judge scores
  - POST runs + summary back to the Go server worker callback

Credentials are read from env only (loaded by the server from .env); never printed or stored.
"""
import argparse, concurrent.futures, json, os, re, sys, time, urllib.request, urllib.error

GO = os.environ.get("GO_SERVER", "http://localhost:8000")
# The server generates this per boot and passes it only to its own child
# processes, so the worker can call back even when AUTH_MODE=required.
INTERNAL_TOKEN = os.environ.get("EVALFORGE_INTERNAL_TOKEN", "")


def log(msg):
    """Progress goes to stderr so the server log shows where a run is."""
    print(f"[worker {time.strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)
API = GO + "/api/v1"


def _headers(extra=None):
    h = dict(extra or {})
    if INTERNAL_TOKEN:
        h["X-Internal-Token"] = INTERNAL_TOKEN
    return h


def _get(path):
    req = urllib.request.Request(API + path, headers=_headers())
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read().decode())


def _post(path, payload):
    req = urllib.request.Request(API + path, data=json.dumps(payload).encode(),
                                 headers=_headers({"Content-Type": "application/json"}), method="POST")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())


# ---------------- provider ----------------

# The router refuses any model id other than this literal, so it is not configurable.
MODEL = "openrouter/free"


def provider_config():
    return (os.environ.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
            os.environ.get("OPENROUTER_API_KEY"), MODEL)


def provider_available():
    return bool(provider_config()[1])


def _provider_post(messages, temperature, max_tokens):
    base, key, model = provider_config()
    req = urllib.request.Request(
        base.rstrip("/") + "/chat/completions",
        data=json.dumps({"model": model, "messages": messages, "temperature": temperature,
                         "max_tokens": max_tokens}).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {key}"},
        method="POST")
    with urllib.request.urlopen(req, timeout=90) as r:
        return json.loads(r.read().decode())


def call_llm(messages, temperature=0.0, max_tokens=600, attempts=3):
    """Calls the provider, retrying transient failures and escalating the token
    budget when a reasoning model spends it all before emitting an answer.

    Returns {content, usage, finish_reason, model} or {error, finish_reason}.
    """
    if not provider_available():
        return None
    last = {"error": "no attempt made"}
    for attempt in range(attempts):
        t0 = time.time()
        try:
            data = _provider_post(messages, temperature, max_tokens)
        except urllib.error.HTTPError as e:
            body = e.read()[:200].decode("utf-8", "ignore")
            last = {"error": f"provider HTTP {e.code}: {body}"}
            if e.code in (429, 500, 502, 503, 504) and attempt < attempts - 1:
                time.sleep(1.5 * (attempt + 1))
                continue
            return last
        except Exception as e:
            last = {"error": f"{type(e).__name__}: {e}"}
            if attempt < attempts - 1:
                time.sleep(1.5 * (attempt + 1))
                continue
            return last

        choices = data.get("choices") or [{}]
        choice = choices[0]
        msg = choice.get("message") or {}
        finish = choice.get("finish_reason")
        content = msg.get("content")
        usage = data.get("usage") or {}
        if content and str(content).strip():
            log(f"llm ok attempt={attempt + 1} {time.time() - t0:.1f}s finish={finish} "
                f"model={data.get('model')} max_tokens={max_tokens}")
            return {"content": str(content), "usage": usage, "finish_reason": finish,
                    "model": data.get("model")}
        # `openrouter/free` may route to a reasoning model: if the budget ran out
        # inside the reasoning trace there is no answer yet, so retry with more
        # tokens instead of recording a bogus empty output.
        log(f"llm empty attempt={attempt + 1} {time.time() - t0:.1f}s finish={finish} "
            f"max_tokens={max_tokens}")
        last = {"error": f"empty content (finish_reason: {finish})", "finish_reason": finish}
        if finish == "length" and attempt < attempts - 1:
            max_tokens *= 2
            continue
        return last
    return last


# ---------------- deterministic scorers ----------------

def _norm(v):
    return v.strip().lower() if isinstance(v, str) else json.dumps(v, sort_keys=True, ensure_ascii=False)


def exact_match(sinput, output, expected, config=None):
    if expected is None: return None
    return 1.0 if _norm(output) == _norm(expected) else 0.0


def contains(sinput, output, expected, config=None):
    if expected is None: return None
    exp = _norm(expected if isinstance(expected, str) else json.dumps(expected, ensure_ascii=False))
    out = _norm(output) if isinstance(output, str) else json.dumps(output, ensure_ascii=False)
    return 1.0 if exp in out else 0.0


def levenshtein_similarity(sinput, output, expected, config=None):
    if expected is None: return None
    a = _norm(output if isinstance(output, str) else json.dumps(output, ensure_ascii=False))
    b = _norm(expected if isinstance(expected, str) else json.dumps(expected, ensure_ascii=False))
    if not a and not b: return 1.0
    prev = list(range(len(b) + 1))
    for i, ca in enumerate(a, 1):
        cur = [i]
        for jj, cb in enumerate(b, 1):
            cur.append(min(prev[jj] + 1, cur[jj - 1] + 1, prev[jj - 1] + (ca != cb)))
        prev = cur
    return 1.0 - prev[-1] / max(len(a), len(b))


def json_valid(sinput, output, expected, config=None):
    try:
        json.loads(output if isinstance(output, str) else json.dumps(output))
        return 1.0
    except Exception:
        return 0.0


def numeric_diff(sinput, output, expected, config=None):
    if expected is None: return None
    def num(v):
        m = re.findall(r"-?\d+(?:\.\d+)?", str(v or ""))
        return float(m[0]) if m else None
    a, b = num(output), num(expected)
    if a is None or b is None: return 0.0
    return max(0.0, 1.0 - abs(a - b) / max(abs(b), 1e-9))


def regex_guard(sinput, output, expected, config=None):
    text = output if isinstance(output, str) else json.dumps(output, ensure_ascii=False)
    cfg = config or {}
    try:
        if cfg.get("must_match") and not re.search(cfg["must_match"], text): return 0.0
        if cfg.get("must_not_match") and re.search(cfg["must_not_match"], text): return 0.0
    except re.error:
        return None
    return 1.0


BUILTIN = {f.__name__: f for f in [exact_match, contains, levenshtein_similarity, json_valid, numeric_diff, regex_guard]}

JUDGE_PROMPTS = {
    "factuality_judge": (
        "You are a strict grader. Question: {input}\nExpected answer: {expected}\nGenerated answer: {output}\n"
        "Is the generated answer factually consistent with the expected answer? "
        'Reply ONLY with JSON: {{"score": 0.0-1.0, "rationale": "..."}}'
    ),
    "helpfulness_judge": (
        "You are a grader rating helpfulness. Question: {input}\nGenerated answer: {output}\n"
        'Reply ONLY with JSON: {{"score": 0.0-1.0, "rationale": "..."}}'
    ),
}


def _heuristic_judge(output, expected):
    out = _norm(output if isinstance(output, str) else (json.dumps(output, ensure_ascii=False) if output is not None else ""))
    exp = _norm(expected if isinstance(expected, str) else json.dumps(expected, ensure_ascii=False))
    if not exp:
        return 0.5, "heuristic_fallback: no expected value"
    exp_words = set(re.findall(r"\w+", exp))
    out_words = set(re.findall(r"\w+", out))
    overlap = len(exp_words & out_words) / len(exp_words) if exp_words else 0.0
    return round(min(1.0, overlap), 2), f"heuristic_fallback: lexical overlap {overlap:.2f}"


def run_judge(name, sinput, output, expected, config=None):
    """Returns (score, rationale, mode). mode records how the score was obtained:
    llm | heuristic_fallback | provider_error | unparseable."""
    template = (config or {}).get("template") or JUDGE_PROMPTS.get(name)
    if not template:
        return None, "unknown judge", "error"
    sinput = "" if sinput is None else str(sinput)
    output = "" if output is None else str(output)
    expected = "" if expected is None else (expected if isinstance(expected, str) else json.dumps(expected))
    if provider_available():
        resp = call_llm([{"role": "user", "content": template.format(input=sinput, output=output, expected=expected)}])
        content = (resp or {}).get("content")
        if content:
            text = str(content)
            parsed = {}
            m = re.search(r"\{.*\}", text, re.S)
            if m:
                try:
                    parsed = json.loads(m.group(0))
                except Exception:
                    parsed = {}
            score = None
            if isinstance(parsed, dict) and parsed.get("score") is not None:
                try:
                    score = float(parsed["score"])
                except (TypeError, ValueError):
                    score = None
            if score is None:
                # prose answer like `{"rationale": ...}` or `Score: 0.8`
                mm = re.search(r"[\"']?score[\"']?\s*[:=]\s*([0-9]*\.?[0-9]+)", text, re.I)
                if mm:
                    try:
                        score = float(mm.group(1))
                    except ValueError:
                        score = None
            if score is None:
                # a judge that did not answer must not be recorded as a 0 score
                return None, f"judge unparseable: {text[:200]}", "unparseable"
            rationale = parsed.get("rationale", "") if isinstance(parsed, dict) else ""
            return score, rationale or text[:200], "llm"
        err = (resp or {}).get("error") or "no response"
        return None, f"provider returned no content: {err}", "provider_error"
    s, r = _heuristic_judge(output or "", expected or "")
    return s, r, "heuristic_fallback"


def run_scorer(name, kind, config, sinput, output, expected):
    if kind == "deterministic":
        fn = BUILTIN.get(name)
        if not fn: return None
        s = fn(sinput, output, expected, config)
        return {"name": name, "score": None if s is None else round(float(s), 4),
                "rationale": "deterministic", "judge_mode": "deterministic"}
    s, r, mode = run_judge(name, sinput, output, expected, config)
    if s is None:
        return {"name": name, "score": None, "rationale": r, "judge_mode": mode}
    return {"name": name, "score": round(float(s), 4), "rationale": r, "judge_mode": mode}


# ---------------- thread (conversation) scorers ----------------

THREAD_SCORERS = [
    "thread_completion", "thread_error_free", "thread_turn_consistency",
    "thread_coherence_judge", "thread_context_retention_judge",
]


def _text(v):
    if v is None:
        return ""
    return v if isinstance(v, str) else json.dumps(v, sort_keys=True, ensure_ascii=False)


def _words(v):
    return set(re.findall(r"\w+", _text(v).lower()))


def _jaccard(a, b):
    if not a and not b:
        return 1.0
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def _turn_completed(turn):
    return turn.get("error") is None and turn.get("output") is not None


def thread_completion(turns):
    if not turns:
        return 0.0
    return sum(1 for t in turns if _turn_completed(t)) / len(turns)


def thread_error_free(turns):
    if not turns:
        return 0.0
    return sum(1 for t in turns if t.get("status") != "error" and t.get("error") is None) / len(turns)


def thread_turn_consistency(turns):
    outputs = [_words(t.get("output")) for t in turns if t.get("output") is not None]
    if len(outputs) < 2:
        return 1.0
    vals = [_jaccard(outputs[i - 1], outputs[i]) for i in range(1, len(outputs))]
    return sum(vals) / len(vals)


def thread_context_retention(turns):
    """Heuristic fallback: later answers should share vocabulary with earlier
    user turns. It is deliberately labelled heuristic, not an LLM judgement."""
    if len(turns) < 2:
        return 1.0
    earlier = set()
    vals = []
    for i, turn in enumerate(turns):
        if i > 0:
            vals.append(_jaccard(_words(turn.get("output")), earlier))
        earlier |= _words(turn.get("input"))
    return sum(vals) / len(vals) if vals else 1.0


def _transcript(turns, limit=8000):
    lines = []
    for i, turn in enumerate(turns):
        lines.append(f"Turn {i + 1} user: {_text(turn.get('input'))[:1200]}")
        lines.append(f"Turn {i + 1} assistant: {_text(turn.get('output'))[:1200]}")
    return "\n".join(lines)[:limit]


THREAD_JUDGE_PROMPTS = {
    "thread_coherence_judge": (
        "You are grading a multi-turn conversation. Does the assistant stay coherent and "
        "consistent across turns, without contradicting itself or losing the thread of the "
        "conversation?\n\n{transcript}\n\n"
        'Reply ONLY with JSON: {{"score": 0.0-1.0, "rationale": "..."}}'
    ),
    "thread_context_retention_judge": (
        "You are grading whether a multi-turn assistant retains context from earlier turns "
        "(names, constraints, decisions, and the user's goal).\n\n{transcript}\n\n"
        'Reply ONLY with JSON: {{"score": 0.0-1.0, "rationale": "..."}}'
    ),
}


def _parse_judge(content):
    parsed = {}
    m = re.search(r"\{.*\}", str(content), re.S)
    if m:
        try:
            parsed = json.loads(m.group(0))
        except Exception:
            parsed = {}
    score = None
    if isinstance(parsed, dict) and parsed.get("score") is not None:
        try:
            score = float(parsed["score"])
        except (TypeError, ValueError):
            score = None
    if score is None:
        mm = re.search(r"[\"']?score[\"']?\s*[:=]\s*([0-9]*\.?[0-9]+)", str(content), re.I)
        if mm:
            try:
                score = float(mm.group(1))
            except ValueError:
                score = None
    return score, (parsed.get("rationale", "") if isinstance(parsed, dict) else "")


def run_thread_judge(name, turns):
    fallback_reason = "heuristic_fallback: no provider key"
    if provider_available():
        prompt = THREAD_JUDGE_PROMPTS[name].format(transcript=_transcript(turns))
        resp = call_llm([{"role": "user", "content": prompt}])
        content = (resp or {}).get("content")
        if content:
            score, rationale = _parse_judge(content)
            if score is None:
                return None, f"judge unparseable: {str(content)[:200]}", "unparseable"
            return score, rationale or str(content)[:200], "llm"
        # A configured-but-broken provider (bad key, quota, outage) should not
        # leave the conversation unscored; deterministic heuristics still give
        # an actionable signal, clearly labelled as a fallback.
        fallback_reason = f"heuristic_fallback: provider error ({(resp or {}).get('error')})"
    if name == "thread_context_retention_judge":
        s = thread_context_retention(turns)
    else:
        s = 0.5 * thread_completion(turns) + 0.5 * thread_turn_consistency(turns)
    return s, fallback_reason, "heuristic_fallback"


def run_thread_scorer(name, turns):
    if name == "thread_completion":
        s, r, mode = thread_completion(turns), "fraction of turns with an answer", "deterministic"
    elif name == "thread_error_free":
        s, r, mode = thread_error_free(turns), "fraction of turns without an error", "deterministic"
    elif name == "thread_turn_consistency":
        s, r, mode = thread_turn_consistency(turns), "lexical consistency between consecutive answers", "deterministic"
    elif name in THREAD_JUDGE_PROMPTS:
        s, r, mode = run_thread_judge(name, turns)
    else:
        return None
    return {"name": name, "score": None if s is None else round(float(s), 4),
            "rationale": r, "judge_mode": mode, "source": "worker"}


def run_thread_eval(args):
    try:
        detail = _get(f"/projects/{args.project_id}/threads/{args.thread_id}")
        turns = detail.get("turns") or []
        names = [s.strip() for s in (args.scorers or "").split(",") if s.strip()] or THREAD_SCORERS
        log(f"start thread={args.thread_id} turns={len(turns)} scorers={names}")
        scores = [sc for sc in (run_thread_scorer(n, turns) for n in names) if sc]
        acc = [s["score"] for s in scores if s.get("score") is not None]
        summary = {
            "turns": len(turns),
            "scores": {s["name"]: s["score"] for s in scores},
            "mean": round(sum(acc) / len(acc), 4) if acc else None,
        }
        if args.job_id:
            _post(f"/worker_callback/thread/{args.job_id}",
                  {"scores": scores, "summary": summary, "status": "completed"})
        log(f"done thread={args.thread_id} scores={len(scores)}")
    except Exception as e:
        import traceback
        log(f"failed thread={args.thread_id}: {e}")
        if args.job_id:
            try:
                _post(f"/worker_callback/thread/{args.job_id}",
                      {"scores": [], "summary": {"error": str(e),
                       "trace": traceback.format_exc()[-800:]}, "status": "failed", "error": str(e)})
            except Exception as post_err:
                log(f"thread callback failed: {post_err}")
        raise


# ---------------- task output ----------------

def render_prompt(template, record_input):
    text = record_input if isinstance(record_input, str) else json.dumps(record_input, ensure_ascii=False)
    if isinstance(template, dict) and "messages" in template:
        return [{**m, "content": m.get("content", "").replace("{{input}}", text).replace("{input}", text)}
                for m in template["messages"]]
    return [{"role": "user", "content": str(template).replace("{{input}}", text).replace("{input}", text)}]


def _simulate_output(record_input, expected, salt=""):
    text = record_input if isinstance(record_input, str) else json.dumps(record_input, ensure_ascii=False)
    h = (sum(ord(c) for c in text) + sum(ord(c) for c in salt)) % 10
    if expected is not None:
        exp = expected if isinstance(expected, str) else json.dumps(expected)
        if h < 4: return exp
        if h < 6: return f"{exp} (per additional context)"
        if len(exp.split()) > 3: return " ".join(reversed(exp.split()))
        return f"I'm not sure about {text}."
    return f"Computed answer for {text}: variant {h}"


def generate_task(record_input, task_mode, salt, prompt_template=None):
    """Returns (output, error, mode) where mode is llm | simulated_fallback."""
    if provider_available():
        if prompt_template is not None:
            msgs = render_prompt(prompt_template, record_input)
        else:
            msgs = render_prompt({"messages": [
                {"role": "user", "content": "Answer this support question accurately and concisely:\n{{input}}"}]}, record_input)
        resp = call_llm(msgs)
        content = (resp or {}).get("content")
        if content:
            return content, None, "llm"
        reason = (resp or {}).get("error") or "no content"
        return (_simulate_output(record_input, None, salt=salt),
                f"provider returned no content ({reason}); simulated fallback used",
                "simulated_fallback")
    return _simulate_output(record_input, None, salt=salt), None, "simulated_fallback"


# ---------------- online evaluation ----------------

def _as_text(v):
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    return json.dumps(v, ensure_ascii=False)


def run_online(args):
    """Score one live trace for an online rule and print the result as JSON.

    The server owns rule matching, sampling and persistence; this mode only
    executes the scorers the rule selected. Progress goes to stderr and the
    single JSON document to stdout, so the caller needs no callback protocol.
    """
    if not args.trace_id:
        raise SystemExit("--trace-id is required with --online")
    detail = _get(f"/projects/{args.project_id}/traces/{args.trace_id}")
    roots = detail.get("span_tree") or []
    if not roots:
        raise SystemExit(f"trace {args.trace_id} has no spans")
    root = roots[0]
    sinput, output = _as_text(root.get("input")), _as_text(root.get("output"))
    names = [s.strip() for s in (args.scorers or "").split(",") if s.strip()]
    kinds = {s.get("name"): s.get("kind", "deterministic") for s in _get("/scorers")}
    results = []
    for name in names:
        kind = kinds.get(name)
        if kind is None:
            log(f"online: unknown scorer {name}, skipping")
            continue
        res = run_scorer(name, kind, {}, sinput, output, "")
        if res:
            results.append(res)
    log(f"online trace={args.trace_id} scorers={len(results)}")
    print(json.dumps({"scores": results}), flush=True)


# ---------------- main ----------------

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--experiment-id", default="")
    ap.add_argument("--project-id", required=True)
    ap.add_argument("--dataset-id", default="")
    ap.add_argument("--dataset-version-id", default="")
    ap.add_argument("--prompt-version-id", default="")
    ap.add_argument("--experiment-name")
    ap.add_argument("--baseline-id", default="")
    ap.add_argument("--thread-id", default="")
    ap.add_argument("--job-id", default="")
    ap.add_argument("--online", action="store_true")
    ap.add_argument("--trace-id", default="")
    ap.add_argument("--scorers", default="")
    args = ap.parse_args()

    if args.online:
        run_online(args)
        return
    if args.thread_id:
        run_thread_eval(args)
        return
    if not args.experiment_id or not args.dataset_id:
        ap.error("--experiment-id and --dataset-id are required for dataset evaluations")

    try:
        # An experiment pins an immutable snapshot, so a concurrent dataset edit
        # cannot change what this run evaluates. Older jobs without a version id
        # fall back to the live dataset.
        if args.dataset_version_id:
            ds = _get(f"/dataset_versions/{args.dataset_version_id}")
        else:
            ds = _get(f"/datasets/{args.dataset_id}")
        records = ds["records"]
        scorer_names = [s.strip() for s in args.scorers.split(",") if s.strip()]
        if not scorer_names:
            scorer_names = ["exact_match", "factuality_judge"]
        builtin = {b["name"]: b["kind"] for b in _get("/scorers")}

        prompt_template = None
        if args.prompt_version_id:
            prompt = _get(f"/prompts/{args.prompt_version_id}")
            prompt_template = prompt.get("template")
            log(f"prompt name={prompt.get('name')} version={prompt.get('version')}")

        # baseline runs for comparison
        base_by_input = {}
        if args.baseline_id:
            try:
                bed = _get(f"/experiments/{args.baseline_id}")
                for r in bed.get("runs", []):
                    key = json.dumps(r.get("input"), sort_keys=True, ensure_ascii=False) if not isinstance(r.get("input"), str) else r["input"]
                    base_by_input[key] = r.get("scores") or {}
            except Exception:
                pass

        log(f"start experiment={args.experiment_id} records={len(records)} scorers={scorer_names}")

        def run_record(i, rec):
            """One dataset case: generate the task output, then apply every scorer.
            Records are independent, so they run concurrently (the free router
            answers a single call in seconds but with a long tail)."""
            sinput = rec.get("input")
            expected = rec.get("expected")
            t0 = time.time() * 1000
            out, err, out_mode = generate_task(sinput, "auto", salt=args.experiment_id + str(i),
                                               prompt_template=prompt_template)
            latency = int(time.time() * 1000 - t0)
            scores = {}
            for sn in scorer_names:
                kind = builtin.get(sn, "deterministic")
                res = run_scorer(sn, kind, {}, json.dumps(sinput if not isinstance(sinput, str) else sinput), out, expected)
                if res:
                    scores[res["name"]] = {"score": res["score"], "rationale": res["rationale"],
                                           "judge_mode": res.get("judge_mode", "deterministic")}
            log(f"record {i + 1}/{len(records)} mode={out_mode} {latency}ms")
            return {
                "record_id": rec.get("id"), "input": sinput, "expected": expected,
                "output": out, "error": err, "latency_ms": latency, "output_mode": out_mode,
                "scores": scores,
            }

        with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, min(4, len(records)))) as pool:
            runs_out = list(pool.map(lambda a: run_record(*a), list(enumerate(records))))

        # summary
        score_acc = {}
        for r in runs_out:
            for name, s in r["scores"].items():
                if s.get("score") is not None:
                    score_acc.setdefault(name, []).append(s["score"])
        summary_scores = {k: {"mean": round(sum(v) / len(v), 4), "count": len(v)} for k, v in score_acc.items()}

        comparison = None
        if args.baseline_id and base_by_input:
            improve, regress, per = {}, {}, {}
            for r in runs_out:
                key = json.dumps(r["input"], sort_keys=True, ensure_ascii=False) if not isinstance(r["input"], str) else r["input"]
                bsc = base_by_input.get(key, {})
                for name, s in r["scores"].items():
                    if s.get("score") is None: continue
                    bv = (bsc.get(name) or {}).get("score")
                    if bv is None: continue
                    d = s["score"] - bv
                    per.setdefault(name, []).append(d)
                    if d > 0.05: improve[name] = improve.get(name, 0) + 1
                    elif d < -0.05: regress[name] = regress.get(name, 0) + 1
            comparison = {"baseline": args.baseline_id,
                          "scores": {k: {"mean": round(sum(v) / len(v), 4),
                                         "improvements": improve.get(k, 0),
                                         "regressions": regress.get(k, 0)}
                                     for k, v in per.items()}}
        summary = {"runs": len(runs_out), "scores": summary_scores, "comparison": comparison}

        _post(f"/worker_callback/{args.experiment_id}", {"runs": runs_out, "summary": summary, "status": "completed"})
        log(f"done experiment={args.experiment_id} runs={len(runs_out)}")
    except Exception as e:
        import traceback
        log(f"failed experiment={args.experiment_id}: {e}")
        _post(f"/worker_callback/{args.experiment_id}",
              {"runs": [], "summary": {"error": str(e), "trace": traceback.format_exc()[-800:]}, "status": "failed"})


if __name__ == "__main__":
    main()
