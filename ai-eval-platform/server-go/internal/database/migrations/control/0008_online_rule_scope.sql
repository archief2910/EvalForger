-- 0008_online_rule_scope.sql (control plane)
-- An online rule can watch a single trace or a whole conversation.
--
-- A trace rule scores one request: "this response was wrong". A thread rule
-- scores a conversation when it closes: "the agent forgot the user's name
-- after turn four". The second question cannot be answered by looking at a
-- trace, which is why the scope lives on the rule rather than being inferred
-- from the scorers.
--
-- The scope also decides what an alert points at, so alerts carry the subject
-- they fired on rather than assuming a trace.

ALTER TABLE online_rules ADD COLUMN scope TEXT NOT NULL DEFAULT 'trace';

-- Alerts gain a typed subject. Existing rows are trace-scoped, so the backfill
-- copies the trace id across; a nullable column would leave half the history
-- unaddressable.
ALTER TABLE online_alerts ADD COLUMN subject_type TEXT NOT NULL DEFAULT 'trace';
ALTER TABLE online_alerts ADD COLUMN subject_id TEXT NOT NULL DEFAULT '';
UPDATE online_alerts SET subject_id = trace_id WHERE subject_id = '';

-- The old uniqueness was (rule_id, trace_id, scorer). A thread alert has no
-- trace id, so two different conversations breaching the same rule would
-- collide on the empty string. The subject replaces the trace id as the thing
-- an alert is about.
ALTER TABLE online_alerts DROP CONSTRAINT IF EXISTS online_alerts_rule_id_trace_id_scorer_key;
ALTER TABLE online_alerts DROP CONSTRAINT IF EXISTS online_alerts_rule_id_trace_id_scorer_unique;
DROP INDEX IF EXISTS online_alerts_rule_id_trace_id_scorer_idx;
CREATE UNIQUE INDEX IF NOT EXISTS idx_online_alerts_subject
  ON online_alerts(rule_id, subject_type, subject_id, scorer);
