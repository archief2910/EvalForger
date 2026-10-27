"""Runner for a user-supplied EvalForge metric.

The contract is deliberately tiny, because every rule here is one a metric
author has to know about:

    def score(payload: dict) -> float | dict
        return 0.82
        # or
        return {"score": 0.82, "rationale": "..."}

`payload` contains the case being scored. For a trace it is
`{"input": ..., "output": ..., "expected": ...}`; for a conversation it is
`{"turns": [...]}`.

The class form is accepted too, for metrics ported from Opik:

    class Metric:
        def score(self, **kwargs) -> float | dict

Everything the runner itself prints is one JSON document on stdout. A metric's
own print() statements are captured and folded into the rationale, so a
careless `print` cannot corrupt the protocol.

Run as: python runner.py <directory containing scorer.py and payload.json>
(the container form passes an absolute /sandbox path).
"""

import json
import os
import sys
import traceback


def _result(value):
    """Normalise whatever the metric returned into {score, rationale}."""
    if isinstance(value, dict):
        score = value.get("score", value.get("value"))
        rationale = value.get("rationale") or value.get("reason") or ""
        return score, str(rationale)
    return value, ""


def _user_facing(exc):
    """The cause first, then the user's frames only.

    The runner's own frames are noise: they tell the metric author nothing and
    they leak the harness into the message.
    """
    cause = "".join(traceback.format_exception_only(type(exc), exc)).rstrip()
    frames = "".join(traceback.format_tb(exc.__traceback__)).rstrip()
    return cause if not frames else cause + "\n" + frames


def main():
    directory = sys.argv[1] if len(sys.argv) > 1 else "/sandbox"
    with open(os.path.join(directory, "payload.json"), "r", encoding="utf-8") as f:
        payload = json.load(f)
    with open(os.path.join(directory, "scorer.py"), "r", encoding="utf-8") as f:
        code = f.read()

    namespace = {"__name__": "user_metric"}
    try:
        compiled = compile(code, "<metric>", "exec")
        exec(compiled, namespace)
    except Exception as exc:  # noqa: BLE001 - the message is the product
        print(json.dumps({"error": "invalid Python code: " + _user_facing(exc)}))
        return 1

    try:
        if callable(namespace.get("score")):
            value = namespace["score"](payload)
        else:
            metric_class = None
            for name, obj in namespace.items():
                if isinstance(obj, type) and name != "object":
                    if hasattr(obj, "score") and callable(getattr(obj, "score")):
                        metric_class = obj
                        break
            if metric_class is None:
                print(json.dumps({
                    "error": "define a score(payload) function or a class with score(**kwargs)"
                }))
                return 1
            instance = metric_class()
            if isinstance(payload, dict):
                value = instance.score(**payload)
            else:
                value = instance.score(payload)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": "metric raised: " + _user_facing(exc)}))
        return 1

    score, rationale = _result(value)
    if score is None:
        print(json.dumps({"error": "metric returned no score"}))
        return 1
    try:
        score = float(score)
    except (TypeError, ValueError):
        print(json.dumps({"error": f"score {score!r} is not a number"}))
        return 1
    print(json.dumps({"score": score, "rationale": rationale}))
    return 0


if __name__ == "__main__":
    sys.exit(main())