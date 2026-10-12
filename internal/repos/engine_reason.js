// reason.js — the reasoning engine's loop (expert spike, E02).
//
// The harness (this script) owns the loop's mechanics: the read set, the
// frontier of moves, the backtrack stack, the budgets, the posterior and the
// writes. The model owns every judgment inside it — relevance, support,
// which move next, the drafted claim, whether it answers, the hypothesis and
// what would settle it — each ONE knomit.model call with a closed schema
// whose `reasoning` field comes first (E07 §3). A judgment that fails (a
// thrown call: truncated, off-schema, unreachable) is logged by the host,
// recorded in the chain, and read as the safe default (not relevant,
// unrelated, the top-ranked move, no claim, "not"), so a bad reply never
// ends the run as an error: the run still reaches an answer or a gap.
//
// Globals (frozen): question (string); run {id, budget_ms, max_moves,
// max_steps, trust}; world {repo, repo_id, branch, writable, root};
// ontology {context: {<topic path>: [declared context keys]}, topics:
// [top-level topic names]}. Host: knomit.query, explain, learn, incoming,
// experiment, model — each takes a `log` object ({step, state, move, from,
// arg, judgment, posterior, facts}) the host copies into the run's log line.
//
// The world is the asker's binding: HEAD of it is what we know now, explain
// is how we came to know it. A superseded fact is history, never error; a
// retracted one is history, never evidence. Writes go into an experiment
// this script opens and never commits (accepting is the asker's).
//
// States (E02 §1):
//   GATHER      neighbours(question)                                   -> ASSESS
//   ASSESS      relevance, then support per relevant fact (model);
//               a line resting on a retracted or contradicted premise  -> BACKTRACK
//               otherwise                                              -> WIDEN
//   WIDEN       the model picks one move from the ranked frontier,
//               the harness runs it                                    -> ASSESS
//               the model stops, or the move budget is spent           -> SYNTHESIZE
//   BACKTRACK   supersession(f) confirms the dead premise; the line is
//               popped off the stack; the model picks the next frontier
//               entry (backtrack_choice)                               -> ASSESS
//   SYNTHESIZE  the model drafts the claim from the supports          -> DECIDE
//   DECIDE      the model judges whether it answers; the harness
//               computes the posterior                                 -> EMIT | HYPOTHESIZE | GAP
//   HYPOTHESIZE the model names the missing premise and its settlement -> SEEK
//   SEEK        the model writes the fact that would settle it; the
//               harness searches by it                                 -> ASSESS
//   EMIT        re-explain every support (freshness), then write       -> done
//   GAP         structured gap + an evidence-free hypothesis fact      -> done
//
// The completion value (the last expression) is the result the host
// returns: {outcome (answer | gap), posterior, chain, gaps, existing_answer?,
// unwritten_facts?}.

// --- the type-semantics table (E02 §4) -------------------------------------
// role: evidence (weighed: enters support and the posterior), premise (the
// epistemic TYPE principle only — an a-priori definition, weighed like
// evidence and applied without re-deriving it; the principles/ topic has no
// standing), open (hypothesis: no evidence mass, triggers SEEK), instructs
// (methodology: HOW to reason; goes into the briefings and the refs), history
// (never live), constrains (policy) and biases (heuristic): obeyed, never
// weighed, excluded (signal). derived marks types whose weight composes
// through their members (a synthesis is represented by the members it cites
// when they are in the support set too). rank biases the frontier.
var TYPES = {
  observation: { role: "evidence", rank: 1.0 },
  reference: { role: "evidence", rank: 1.0 },
  process: { role: "evidence", rank: 1.0 },
  concept: { role: "evidence", rank: 0.8 },
  pattern: { role: "evidence", rank: 0.8 },
  synthesis: { role: "evidence", rank: 1.2, derived: true },
  insight: { role: "evidence", rank: 1.1, derived: true },
  principle: { role: "premise", rank: 1.0 },
  hypothesis: { role: "open", rank: 0.5 },
  methodology: { role: "instructs", rank: 0.7 },
  policy: { role: "constrains", rank: 0.5 },
  heuristic: { role: "biases", rank: 0.4 },
  signal: { role: "excluded", rank: 0 },
};

// Request properties (the asker's, per question; E02 §2, §5). Defaults come
// from Go (ReasonOptions); these fallbacks only cover a direct script run.
var TRUST = run.trust || 0.5;
var MAX_MOVES = run.max_moves || 12;
var MAX_STEPS = run.max_steps || 60;

// Briefing-size and loop-shape bounds for a small model (E02 §6), not corpus
// properties.
var QUERY_LIMIT = 8; // facts one read returns
var JUDGE_BATCH = 8; // facts under one relevance or support judgment
var MENU_SIZE = 12; // frontier entries the model chooses among
var MENU_PER_MOVE = 4; // at most this many entries of one move kind on a menu
var MAX_LINES = 4; // depth of the backtrack stack
var MAX_BACKTRACKS = 3; // pops before the run stops exploring
var MAX_SEEKS = 2; // targeted searches per run
var MAX_FRESHNESS = 2; // re-judging rounds when a support moved under EMIT
var BODY_CHARS = 1200; // a fact's body in a briefing
var SNIPPET_CHARS = 240; // a fact's snippet in a relevance briefing
var REASONING_CHARS = 1200; // the reasoning budget of one judgment

// --- judgment schemas (closed; reasoning first; additionalProperties false) -
function schemaOf(props, required) {
  var p = { reasoning: { type: "string", maxLength: REASONING_CHARS } };
  for (var k in props) p[k] = props[k];
  return { type: "object", additionalProperties: false, required: ["reasoning"].concat(required), properties: p };
}

var RELEVANT_SCHEMA = schemaOf(
  {
    labels: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["fact", "relevant"],
        properties: { fact: { type: "integer", minimum: 1 }, relevant: { type: "string", enum: ["yes", "no"] } },
      },
    },
  },
  ["labels"]
);

var SUPPORT_SCHEMA = schemaOf(
  {
    verdicts: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["fact", "verdict"],
        properties: {
          fact: { type: "integer", minimum: 1 },
          verdict: { type: "string", enum: ["supports", "contradicts", "restates", "unrelated"] },
        },
      },
    },
  },
  ["verdicts"]
);

function choiceSchema(n) {
  return schemaOf({ choice: { type: "integer", minimum: 0, maximum: n } }, ["choice"]);
}

var SYNTHESIZE_SCHEMA = schemaOf(
  {
    claim: { type: "string" },
    consequence: { type: "string" },
    misreading: { type: "string" },
    carriers: { type: "array", items: { type: "integer", minimum: 1 } },
  },
  ["claim", "consequence", "misreading", "carriers"]
);

var ASSESS_SCHEMA = schemaOf(
  { assess: { type: "string", enum: ["answers", "partly", "not"] }, missing: { type: "string" } },
  ["assess", "missing"]
);

var HYPOTHESIZE_SCHEMA = schemaOf(
  { claim: { type: "string" }, settlement: { type: "string" }, observable: { type: "string" } },
  ["claim", "settlement", "observable"]
);

var SEEK_SCHEMA = schemaOf({ search: { type: "string" } }, ["search"]);

var COMMON =
  "You work inside a knowledge base. Use ONLY the facts given; never your own knowledge. " +
  "Kinds: an epistemic fact is evidence to weigh; a pragmatic fact (policy, heuristic) is a rule to obey, never evidence. " +
  "A hypothesis is an open prediction and is never evidence. A methodology fact tells you HOW to reason: follow it. " +
  "Think briefly in `reasoning` first, then give the answer fields. Reply with JSON only.";

var SYSTEM = {
  relevant:
    COMMON +
    " Task: label each numbered fact `yes` when it bears on the question — it could be part of an answer, could undercut one, " +
    "or tells you how to reason about one — and `no` otherwise (same words or same subject but a different claim is `no`).",
  support:
    COMMON +
    " Task: for each numbered fact give one verdict about the question: supports (evidence for an answer), contradicts " +
    "(evidence against the answer the other facts point to), restates (it already IS the whole answer), unrelated.",
  choose_move:
    COMMON +
    " Task: choose the ONE next move from the numbered menu that is most likely to surface evidence the answer still needs, " +
    "or 0 to stop when the gathered support already answers the question or no move can help. Each move names what it reads.",
  backtrack_choice:
    COMMON +
    " Task: the line of reasoning named below was abandoned because it rests on a premise that is retracted or contradicted. " +
    "Choose the ONE move from the numbered menu that best opens another line, or 0 when none can.",
  synthesize:
    COMMON +
    " Task: draft the answer as ONE claim sentence built only from the supporting facts, keeping every condition they state " +
    "in full. consequence: what a reader should do, or never do, because the claim is true (empty if none). misreading: the " +
    "attractive false reading a hurried reader would take (empty if none). carriers: the numbers of the facts whose conditions " +
    "the claim carries. Leave claim empty when the facts do not answer the question.",
  assess:
    COMMON +
    " Task: does the drafted claim, resting on the numbered supporting facts, answer the question AS ASKED? answers, partly, " +
    "or not. In missing, state as one claim the premise that would be needed to answer it, or empty when nothing is missing.",
  hypothesize:
    COMMON +
    " Task: the facts do not answer the question. State the missing premise as a falsifiable prediction (claim), what evidence " +
    "would settle it (settlement), and the observation that would confirm or refute it (observable).",
  seek:
    COMMON +
    " Task: write, as one sentence phrased like a fact in this knowledge base, the fact that would settle the hypothesis if " +
    "it existed. It is used as a search text, so name the subjects and the measured quantity.",
};

// --- state ------------------------------------------------------------------
var st = {
  step: 0,
  moves: 0,
  posterior: 0,
  facts: {}, // path -> fact record (see addFact)
  order: [], // paths in read order (the "rank" of a fact)
  pending: [], // paths surfaced by the last read, not yet judged
  frontier: [], // {key, move, from, arg, count, origins, score}
  done: {}, // move key -> true
  lines: [], // the backtrack stack: {anchor, opened_at}
  dead: {}, // path -> why: abandoned lines and everything resting only on them
  premiseCheck: "", // a derived fact whose refs_out just ran
  pendingBacktrack: null, // {from, premise, cause}
  backtracks: 0,
  seeks: 0,
  seekQueue: [], // settlement texts of relevant open hypotheses
  freshness: 0,
  draft: null, // {claim, consequence, misreading, carriers: [paths]}
  assess: "",
  missing: "",
  hypothesis: null, // {claim, settlement, observable}
  abandoned: [], // [{anchor, premise, cause}]
  chain: [],
  tried: [],
  gaps: [],
  existing: "",
  unwritten: [],
  outcome: "",
  final: false,
};

// --- small helpers ----------------------------------------------------------
function mv(name, from, arg) {
  return { move: name, from: from || "", arg: arg || "" };
}

function moveKey(m) {
  return m.move + "(" + (m.from || m.arg || "") + ")";
}

function logOf(state, m, judgment, facts) {
  var l = { step: st.step, state: state, move: m.move, from: m.from, arg: m.arg, posterior: st.posterior };
  if (judgment) l.judgment = judgment;
  if (facts) l.facts = facts.slice(0, 5);
  return l;
}

// record appends one chain entry. Exploration moves (the scenario format's
// move names) are also the "moves taken" every later briefing lists.
function record(state, m, facts, judgment, verdict, support) {
  var e = { step: st.step, state: state, facts: (facts || []).slice(0, 5), posterior: st.posterior };
  if (m && m.move) e.move = m.move;
  if (m && m.from) e.from = m.from;
  if (m && m.arg) e.arg = m.arg;
  if (judgment) e.judgment = judgment;
  if (verdict) e.verdict = verdict;
  if (support) e.support = support;
  st.chain.push(e);
  if (m && m.move && !judgment) st.tried.push(moveKey(m));
}

function uniq(list) {
  var seen = {},
    out = [];
  list.forEach(function (x) {
    if (x && !seen[x]) {
      seen[x] = true;
      out.push(x);
    }
  });
  return out;
}

function slug(text, max) {
  var s = String(text).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (s.length > max) s = s.slice(0, max).replace(/-+$/, "");
  return s || "question";
}

function errText(e) {
  return String((e && e.message) || e);
}

// A host call that failed because the run's budget ran out is not a bad
// judgment: rethrow it so the run ends as interrupted.
function isDeadline(e) {
  var m = errText(e);
  return m.indexOf("context deadline exceeded") >= 0 || m.indexOf("context canceled") >= 0;
}

// localPath maps a path as a tool returned it onto the write repo's own form:
// kb://<this repo id>/<path> -> <path>. A ref into another mount keeps its
// kb:// form (it is read, cited, never filed under).
function localPath(p) {
  p = String(p || "");
  var pre = "kb://" + world.repo_id + "/";
  if (p.indexOf(pre) === 0) return p.slice(pre.length);
  return p;
}

function typeOf(f) {
  return TYPES[f.type] ? f.type : "observation";
}

function roleOf(f) {
  if (f.kind === "pragmatic") {
    var r = TYPES[typeOf(f)].role;
    return r === "biases" || r === "excluded" ? r : "constrains";
  }
  return TYPES[typeOf(f)].role;
}

function isDerived(f) {
  return !!TYPES[typeOf(f)].derived;
}

function stripFrontmatter(body) {
  var b = String(body || "");
  if (b.indexOf("---\n") === 0) {
    var end = b.indexOf("\n---\n", 4);
    if (end > 0) b = b.slice(end + 5);
  }
  return b.replace(/^\s*#[^\n]*\n+/, "").trim();
}

function firstSentence(text) {
  var t = String(text || "").replace(/\s+/g, " ").trim();
  var i = t.search(/[.!?](\s|$)/);
  return i > 0 ? t.slice(0, i + 1) : t.slice(0, 200);
}

// --- the read set -------------------------------------------------------------
// addFact records one fact a read surfaced, from a query row, an explain node
// or an incoming row, with WHERE it came from (origin: a fact path, or
// "question" for reads the question itself drove). A fact reached only
// through facts that are later abandoned is abandoned with them.
function addFact(raw, origin, how) {
  var path = localPath(raw.file || raw.path);
  if (!path || path.indexOf(".knomit/") === 0 || raw.kind === "system_file") return null;
  var fm = raw.frontmatter || {};
  var f = st.facts[path];
  var isNew = !f;
  if (isNew) {
    f = {
      path: path,
      title: raw.title || path,
      type: raw.type || "observation",
      kind: raw.kind || "epistemic",
      confidence: 0.5,
      sources: 1,
      domain: [],
      entities: [],
      motifs: [],
      refs: [],
      body: "",
      commit: raw.commit || "",
      evidence_weight: 0,
      via: {},
      how: how,
      relevant: "",
      verdict: "",
      hydrated: false,
      children: [],
      revisions: 0,
      deleted: !!raw.deleted,
      superseded: !!raw.superseded,
      history: null,
    };
    st.facts[path] = f;
    st.order.push(path);
  }
  var src = raw.frontmatter ? fm : raw;
  var conf = raw.confidence !== undefined ? raw.confidence : fm.confidence;
  if (typeof conf === "number" && (isNew || !f.hydrated)) f.confidence = conf;
  if (typeof src.sources === "number" && src.sources > 0) f.sources = src.sources;
  if (Array.isArray(src.domain) && src.domain.length) f.domain = src.domain;
  if (Array.isArray(src.entities) && src.entities.length) f.entities = src.entities;
  if (Array.isArray(src.motifs) && src.motifs.length) {
    f.motifs = src.motifs.map(function (m) { return typeof m === "string" ? m : m.motif; });
  }
  if (Array.isArray(src.refs) && src.refs.length && !f.hydrated) f.refs = src.refs;
  if (typeof src.evidence_weight === "number") f.evidence_weight = src.evidence_weight;
  if (raw.body && !f.hydrated) f.body = stripFrontmatter(raw.body);
  if (raw.deleted) f.deleted = true;
  if (raw.superseded) f.superseded = true;
  f.via[origin || "question"] = true;
  return isNew ? path : null;
}

function alive(path) {
  var f = st.facts[path];
  return !!f && !st.dead[path] && !f.deleted;
}

// hydrate reads a fact in full at HEAD of the world (explain without a
// commit: what we know NOW), with its refs, the versions its refs pointed to
// (deleted / superseded flags), and the length of its history. The explain
// is cached: refs_out and history later reuse it.
function hydrate(path, state) {
  var f = st.facts[path];
  if (!f || f.hydrated) return f;
  var ex;
  try {
    ex = explainAll(path, {}, state, mv("read", path));
  } catch (e) {
    if (isDeadline(e)) throw e;
    // gone at HEAD: retracted since it was surfaced; history, never evidence
    f.deleted = true;
    return f;
  }
  absorbExplain(f, ex);
  return f;
}

// explainAll is explain plus the walk's next pages until the root's direct
// refs (depth 1) are all in: the first page of a walk holds the root alone,
// and the walk is breadth-first, so a page that reaches depth 2 (or the end)
// has every depth-1 node.
var EXPLAIN_PAGES = 4;
function explainAll(path, opts, state, m) {
  var o = { log: logOf(state, m) };
  for (var k in opts) o[k] = opts[k];
  var ex = knomit.explain(path, o);
  var nodes = ((ex && ex.facts) || []).slice();
  var pages = 0;
  while (ex && ex.has_more && ex.cursor && pages < EXPLAIN_PAGES) {
    var deep = nodes.some(function (n) { return n.depth >= 2; });
    if (deep) break;
    pages++;
    ex = knomit.explain(path, { cursor: ex.cursor, log: logOf(state, m) });
    nodes = nodes.concat((ex && ex.facts) || []);
  }
  return { facts: nodes };
}

function absorbExplain(f, ex) {
  var nodes = (ex && ex.facts) || [];
  var children = [];
  nodes.forEach(function (n) {
    var p = localPath(n.path);
    if (n.depth === 0 && p === f.path) {
      if (n.title) f.title = n.title;
      if (n.type) f.type = n.type;
      if (n.kind) f.kind = n.kind;
      if (typeof n.confidence === "number") f.confidence = n.confidence;
      if (typeof n.sources === "number" && n.sources > 0) f.sources = n.sources;
      if (n.domain) f.domain = n.domain;
      if (n.entities) f.entities = n.entities;
      if (n.motifs) f.motifs = n.motifs.map(function (m) { return typeof m === "string" ? m : m.motif; });
      if (typeof n.evidence_weight === "number") f.evidence_weight = n.evidence_weight;
      if (n.body) f.body = stripFrontmatter(n.body);
      if (n.commit) f.commit = n.commit;
      f.refs = ((n.refs && n.refs.local) || []).map(localPath);
      var h = n.history || {};
      f.revisions = ((h.revisions || []).length) + (h.more_available ? 1 : 0);
      f.historyRevs = h.revisions || [];
      if (n.deleted) f.deleted = true;
    } else if (n.depth === 1 && n.kind !== "system_file" && p) {
      children.push(n);
    }
  });
  f.children = children;
  f.hydrated = true;
}

// --- the frontier -------------------------------------------------------------
// Each candidate move carries the facts it starts from (origins), a count of
// what it is expected to return, and a score from the moves' counts and the
// type table. A move whose every origin is abandoned leaves the frontier.
var MOVE_BASE = {
  refs_out: 3,
  refs_in: 2.5,
  entity: 2.5,
  motif: 3,
  supersession: 2.5,
  history: 2,
  neighbours: 1.5,
  domain: 1,
  ontology: 1,
  kind_type: 1,
};

function addMove(m, origin, count, bias) {
  var key = moveKey(m);
  if (st.done[key]) return;
  var e = null;
  for (var i = 0; i < st.frontier.length; i++) {
    if (st.frontier[i].key === key) {
      e = st.frontier[i];
      break;
    }
  }
  if (!e) {
    e = { key: key, move: m.move, from: m.from, arg: m.arg, origins: {}, count: 0, bias: 0 };
    st.frontier.push(e);
  }
  e.origins[origin] = true;
  e.count = Math.max(e.count, count || 0);
  e.bias = Math.max(e.bias, bias || 0);
}

function originAlive(o) {
  return o === "question" || alive(o);
}

function liveFrontier() {
  return st.frontier.filter(function (e) {
    for (var o in e.origins) if (originAlive(o)) return true;
    return false;
  });
}

function scoreOf(e) {
  var best = 0;
  for (var o in e.origins) {
    if (!originAlive(o)) continue;
    var r = o === "question" ? 1 : TYPES[typeOf(st.facts[o])].rank;
    var f = st.facts[o];
    if (f && f.verdict === "supports") r += 0.5;
    if (r > best) best = r;
  }
  var n = 0;
  for (var k in e.origins) if (originAlive(k)) n++;
  var bias = e.bias;
  // an axis the question already names was searched by the gather: a
  // subject, regularity or scope the question does NOT name is the new lead
  if ((e.move === "entity" || e.move === "domain" || e.move === "motif") && namedInQuestion(e.arg)) bias -= 1;
  // until a methodology is in the read set, HOW to reason here is worth a read
  if (e.move === "kind_type" && e.arg === "methodology" && !st.order.some(function (q) { return roleOf(st.facts[q]) === "instructs"; })) bias += 4;
  return (MOVE_BASE[e.move] || 1) * best + bias + Math.min(e.count, 6) * 0.25 + (n - 1) * 0.5;
}

var QUESTION_WORDS = (function () {
  var m = {};
  (String(question).toLowerCase().match(/[a-z0-9]+/g) || []).forEach(function (w) { m[w.replace(/'s$/, "")] = true; });
  return m;
})();

function namedInQuestion(arg) {
  var ws = (String(arg).toLowerCase().match(/[a-z0-9]+/g) || []).filter(function (w) { return w.length >= 3; });
  return ws.length > 0 && ws.every(function (w) { return QUESTION_WORDS[w] || QUESTION_WORDS[w.replace(/s$/, "")]; });
}

function rankedFrontier() {
  var live = liveFrontier();
  live.forEach(function (e) { e.score = scoreOf(e); });
  live.sort(function (a, b) { return b.score - a.score; });
  return live;
}

// menu is the top of the ranked frontier, with at most MENU_PER_MOVE entries
// of any one move kind, so the model always sees a spread of axes (refs,
// subjects, regularities, scope, the tree, types) and an answer reachable
// only from a lower-ranked kind of move stays reachable (E02's menu-bias
// risk).
function menu() {
  var out = [],
    per = {};
  rankedFrontier().forEach(function (e) {
    if (out.length >= MENU_SIZE) return;
    if ((per[e.move] || 0) >= MENU_PER_MOVE) return;
    per[e.move] = (per[e.move] || 0) + 1;
    out.push(e);
  });
  return out;
}

// expand puts the moves a relevant fact opens on the frontier.
function expand(f) {
  var p = f.path;
  var localRefs = f.refs.filter(function (r) { return r.indexOf("kb://") !== 0 || r.indexOf("kb://" + world.repo_id + "/") === 0; });
  // a derived fact's weight composes through its members: widen into them first
  if (localRefs.length) addMove(mv("refs_out", p), p, localRefs.length, isDerived(f) ? 2 : 0);
  addMove(mv("refs_in", p), p, 1, 0);
  addMove(mv("neighbours", p), p, 1, 0);
  f.entities.forEach(function (x) { addMove(mv("entity", "", x), p, sharing("entities", x), 0); });
  f.motifs.forEach(function (x) { addMove(mv("motif", "", x), p, sharing("motifs", x), 0); });
  f.domain.forEach(function (x) { addMove(mv("domain", "", x), p, sharing("domain", x), 0); });
  var pl = placeOf(p);
  if (pl && pl.category) addMove(mv("ontology", "", world.root + "/" + pl.topic + "/" + pl.category), p, 1, 0);
  if (f.revisions > 1) addMove(mv("history", p), p, f.revisions, 0.5);
  f.children.forEach(function (c) {
    if (c.superseded && !c.deleted) addMove(mv("supersession", localPath(c.path)), p, 1, 0.5);
  });
  // the kinds that shape a chain: HOW to reason here, and the open predictions
  addMove(mv("kind_type", "", "methodology"), "question", 1, 0);
  addMove(mv("kind_type", "", "hypothesis"), "question", 1, -0.25);
}

// sharing counts the relevant live facts that carry value under key: an
// entity or motif many relevant facts share is the subject's axis.
function sharing(key, value) {
  var n = 0,
    v = String(value).toLowerCase();
  st.order.forEach(function (p) {
    var f = st.facts[p];
    if (!alive(p) || f.relevant !== "yes") return;
    if (f[key].some(function (x) { return String(x).toLowerCase() === v; })) n++;
  });
  return n;
}

// --- judgments ----------------------------------------------------------------
// judge makes ONE model call. A throw is logged by the host at Warn; here it
// is recorded in the chain and returned as null, which each caller reads as
// its safe default.
function judge(name, state, m, facts, user, schema) {
  try {
    var r = knomit.model({
      name: name,
      schema: schema,
      system: SYSTEM[name],
      user: user,
      log: logOf(state, m, name, facts),
    });
    return r.output;
  } catch (e) {
    if (isDeadline(e)) throw e;
    record(state, null, facts, name, "failed: " + errText(e).slice(0, 300));
    return null;
  }
}

function factLine(i, f, full) {
  var head =
    "[" + i + "] " + f.path + " (" + f.kind + "/" + f.type + ", confidence " + f.confidence + ", sources " + f.sources + ") " + f.title;
  if (full && f.body) return head + "\n" + f.body.slice(0, BODY_CHARS);
  if (!full && f.body) return head + "\n" + f.body.slice(0, SNIPPET_CHARS);
  return head;
}

function guidance() {
  var lines = [];
  var meth = livePaths(function (f) { return roleOf(f) === "instructs" && f.relevant === "yes"; });
  var rules = livePaths(function (f) { return (roleOf(f) === "constrains" || roleOf(f) === "biases") && f.relevant === "yes"; });
  if (meth.length) {
    lines.push("", "How to reason here (methodology; follow it):");
    meth.slice(0, 2).forEach(function (p) {
      var f = st.facts[p];
      lines.push("- " + f.title + (f.body ? ": " + f.body.slice(0, 400) : ""));
    });
  }
  if (rules.length) {
    lines.push("", "Rules that constrain the answer (obey; they are not evidence):");
    rules.slice(0, 3).forEach(function (p) { lines.push("- " + st.facts[p].title); });
  }
  return lines;
}

function livePaths(pred) {
  return st.order.filter(function (p) { return alive(p) && pred(st.facts[p]); });
}

function supportPaths() {
  return livePaths(function (f) {
    var r = roleOf(f);
    return (f.verdict === "supports" || f.verdict === "restates") && (r === "evidence" || r === "premise");
  });
}

function contradictPaths() {
  return livePaths(function (f) {
    var r = roleOf(f);
    return f.verdict === "contradicts" && (r === "evidence" || r === "premise");
  });
}

function movesTaken() {
  return st.tried.length ? ["", "Moves taken:"].concat(st.tried.map(function (t) { return "- " + t; })) : [];
}

// judgeRelevance labels every path in paths (in batches). A failed batch
// leaves its facts unlabelled, which reads as not relevant.
function judgeRelevance(paths, state) {
  var out = {};
  for (var i = 0; i < paths.length; i += JUDGE_BATCH) {
    var batch = paths.slice(i, i + JUDGE_BATCH);
    var lines = ["Question: " + question, "", "Facts:"];
    batch.forEach(function (p, j) { lines.push(factLine(j + 1, st.facts[p], false)); });
    var o = judge("relevant", state, mv(""), batch, lines.join("\n"), RELEVANT_SCHEMA);
    var labels = {};
    if (o) {
      var seen = {};
      (o.labels || []).forEach(function (l) {
        var p = batch[l.fact - 1];
        if (!p || seen[p]) return; // an index outside the batch is ignored; a repeat never counts twice
        seen[p] = true;
        labels[p] = l.relevant;
      });
    }
    batch.forEach(function (p) {
      var v = labels[p] || "no";
      st.facts[p].relevant = v;
      out[p] = v;
    });
    if (o) record(state, null, batch, "relevant", countOf(labels, "yes") + " of " + batch.length + " relevant", labels);
  }
  return out;
}

function countOf(m, v) {
  var n = 0;
  for (var k in m) if (m[k] === v) n++;
  return n;
}

// judgeSupport gives every path one verdict against the question, deduped
// per fact (fix-later 3): a fact judged twice keeps one verdict, so it is
// never counted twice in the posterior or the answer's sources. A failed
// batch leaves its facts `unrelated`.
function judgeSupport(paths, state) {
  for (var i = 0; i < paths.length; i += JUDGE_BATCH) {
    var batch = paths.slice(i, i + JUDGE_BATCH);
    var lines = ["Question: " + question, "", "Facts:"];
    batch.forEach(function (p, j) { lines.push(factLine(j + 1, st.facts[p], true)); });
    lines = lines.concat(guidance());
    var o = judge("support", state, mv(""), batch, lines.join("\n"), SUPPORT_SCHEMA);
    var verdicts = {};
    if (o) {
      (o.verdicts || []).forEach(function (v) {
        var p = batch[v.fact - 1];
        if (!p || verdicts[p]) return;
        verdicts[p] = v.verdict;
      });
    }
    batch.forEach(function (p) { st.facts[p].verdict = verdicts[p] || "unrelated"; });
    st.posterior = posteriorOf();
    if (o) {
      record(state, null, batch, "support",
        countOf(verdicts, "supports") + " supports, " + countOf(verdicts, "contradicts") + " contradicts, " + countOf(verdicts, "restates") + " restates",
        verdicts);
    }
  }
}

// chooseFrom shows the model the ranked frontier as a menu and returns the
// chosen entry, or null to stop. A failed choice falls back to the top
// entry: the harness's own ranking, recorded as such.
function chooseFrom(name, state, menu, extra) {
  var lines = ["Question: " + question, ""];
  if (extra) lines = lines.concat(extra, [""]);
  var sup = supportPaths();
  lines.push("Supporting facts so far:");
  if (!sup.length) lines.push("(none)");
  sup.slice(0, 8).forEach(function (p, i) { lines.push(factLine(i + 1, st.facts[p], false)); });
  lines = lines.concat(movesTaken());
  lines.push("", "Menu (0 = stop):");
  menu.forEach(function (e, i) {
    lines.push((i + 1) + ". " + e.key + " — " + describe(e));
  });
  lines = lines.concat(guidance());
  var o = judge(name, state, mv(""), [], lines.join("\n"), choiceSchema(menu.length));
  if (!o) {
    record(state, null, [], name, "harness: the judgment failed, took the top-ranked move " + menu[0].key);
    return menu[0];
  }
  var c = o.choice;
  record(state, null, [], name, c === 0 ? "stop" : menu[c - 1] ? menu[c - 1].key : "out of range: stop");
  if (c >= 1 && c <= menu.length) return menu[c - 1];
  return null;
}

function describe(e) {
  var what = {
    refs_out: "what it rests on",
    refs_in: "what rests on it (cited by)",
    entity: "facts about this subject",
    motif: "facts about other subjects with the same regularity",
    domain: "facts in this scope",
    ontology: "siblings in this part of the tree",
    kind_type: "facts of this type",
    neighbours: "facts that say similar things",
    history: "how belief in it changed",
    supersession: "the current version of a premise that moved on",
  }[e.move];
  return (what || e.move) + (e.count ? " (" + e.count + ")" : "");
}

// --- executing a move ---------------------------------------------------------
function execute(e, state) {
  var m = mv(e.move, e.from, e.arg);
  st.done[e.key] = true;
  st.frontier = st.frontier.filter(function (x) { return x.key !== e.key; });
  st.moves++;
  var origin = e.from || firstLiveOrigin(e);
  var surfaced = [];
  var q = null;
  try {
    switch (e.move) {
      case "refs_out": {
        var f = hydrate(e.from, state);
        f.children.forEach(function (c) {
          var p = addFact(c, e.from, moveKey(m));
          surfaced.push(localPath(c.path));
          if (p) st.pending.push(p);
        });
        st.premiseCheck = e.from;
        break;
      }
      case "refs_in": {
        var inc = knomit.incoming(e.from, { log: logOf(state, m) });
        ((inc && inc.refs) || []).forEach(function (r) {
          if (r.deleted) return;
          var p = addFact(r, e.from, moveKey(m));
          surfaced.push(localPath(r.path));
          if (p) st.pending.push(p);
        });
        break;
      }
      case "history": {
        var hf = hydrate(e.from, state);
        var prev = (hf.historyRevs || [])[1];
        if (prev && prev.commit) {
          var old = knomit.explain(e.from, { commit: prev.commit, log: logOf(state, m) });
          var root = ((old && old.facts) || []).filter(function (n) { return n.depth === 0; })[0];
          if (root) hf.history = { commit: prev.commit, title: root.title, revisions: hf.revisions };
        }
        surfaced.push(e.from);
        break;
      }
      case "supersession": {
        // the current version of a premise a referrer reasoned over an older
        // version of: read it at HEAD, judge it as it stands now
        var sp = localPath(e.from);
        var sf = st.facts[sp];
        if (sf) {
          sf.hydrated = false;
          sf.verdict = "";
          sf.relevant = "";
        } else {
          addFact({ path: sp }, origin, moveKey(m));
        }
        hydrate(sp, state);
        surfaced.push(sp);
        if (st.pending.indexOf(sp) < 0) st.pending.push(sp);
        break;
      }
      default: {
        q = queryFor(e);
        var res = knomit.query(Object.assign(q, { log: logOf(state, m) }));
        ((res && res.facts) || []).forEach(function (row) {
          var p = addFact(row, origin, moveKey(m));
          surfaced.push(localPath(row.file));
          if (p) st.pending.push(p);
        });
      }
    }
  } catch (err) {
    if (isDeadline(err)) throw err;
    record(state, m, [], "", "read failed: " + errText(err).slice(0, 200));
    return;
  }
  record(state, m, surfaced, "", surfaced.length + " facts");
}

function firstLiveOrigin(e) {
  for (var o in e.origins) if (originAlive(o) && o !== "question") return o;
  return "question";
}

function queryFor(e) {
  switch (e.move) {
    case "neighbours": {
      var f = st.facts[e.from];
      return { text: f ? f.title + " " + firstSentence(f.body) : question, limit: QUERY_LIMIT };
    }
    case "entity":
      return { entities: [e.arg], limit: QUERY_LIMIT };
    case "motif":
      return { motifs: [e.arg], limit: QUERY_LIMIT };
    case "domain":
      return { applies_to: [e.arg], limit: QUERY_LIMIT };
    case "ontology":
      return { path: e.arg.replace(/\/*$/, "/"), limit: QUERY_LIMIT };
    case "kind_type":
      return { type: [e.arg], limit: QUERY_LIMIT };
  }
  return { text: question, limit: QUERY_LIMIT };
}

// --- trust ------------------------------------------------------------------
// posteriorOf: the chain's posterior over its live support set (fix-later 4).
//
//   I = the supports, with a derived fact (synthesis, insight) dropped when a
//       fact it cites is itself in the set: its weight composes through its
//       members, never on top of them (E02 §4, mirroring collectEvidence).
//   W = min over I of confidence: the answer rests on every premise, so it
//       is as strong as its weakest one. One support of confidence c gives
//       W = c, so one strong support reaches the trust bar on its own.
//   S = sum over I of confidence x sources; C = the same over the live
//       contradicting facts (SumProduct mass, the corpus's own shape).
//   posterior = W                 when C < S (the contradiction was weighed
//                                 and outweighed; it is named in the body)
//             = W x S / (S + C)   when C >= S (it was not: trust drops to
//                                 at most W/2)
//
// Hypotheses, methodology and pragmatic facts never enter (no evidence mass,
// by the type table). Why min and not a noisy-OR or the old S/(S+1): over
// the pilot scenario worlds' gold confidence bands, the weakest-link shape
// lands inside the band for 169 of 230 answerable questions, a noisy-OR for
// 51 and the product for 75; and S/(S+1) left a single support of
// confidence 0.9 at 0.47, under any sensible bar.
function posteriorOf() {
  var sup = supportPaths();
  if (!sup.length) return 0;
  var inSet = {};
  sup.forEach(function (p) { inSet[p] = true; });
  var indep = sup.filter(function (p) {
    var f = st.facts[p];
    return !(isDerived(f) && f.refs.some(function (r) { return inSet[localPath(r)]; }));
  });
  if (!indep.length) indep = sup;
  var W = 1,
    S = 0,
    C = 0;
  indep.forEach(function (p) {
    var f = st.facts[p];
    W = Math.min(W, f.confidence);
    S += f.confidence * f.sources;
  });
  contradictPaths().forEach(function (p) {
    var f = st.facts[p];
    C += f.confidence * f.sources;
  });
  var post = C >= S && C > 0 ? (W * S) / (S + C) : W;
  return Math.round(Math.max(0, Math.min(1, post)) * 100) / 100;
}

function independentSupports() {
  var sup = supportPaths();
  var inSet = {};
  sup.forEach(function (p) { inSet[p] = true; });
  var indep = sup.filter(function (p) {
    var f = st.facts[p];
    return !(isDerived(f) && f.refs.some(function (r) { return inSet[localPath(r)]; }));
  });
  return indep.length ? indep : sup;
}

// --- backtracking -----------------------------------------------------------
// abandon marks a line dead and propagates: every fact reached ONLY through
// dead facts dies with it, and so does every frontier entry whose every
// origin is dead (liveFrontier filters them).
function abandon(path, why) {
  st.dead[path] = why;
  var changed = true;
  while (changed) {
    changed = false;
    st.order.forEach(function (p) {
      if (st.dead[p]) return;
      var via = Object.keys(st.facts[p].via);
      if (!via.length) return;
      var allDead = via.every(function (o) { return o !== "question" && (st.dead[o] || (st.facts[o] && st.facts[o].deleted)); });
      if (allDead) {
        st.dead[p] = "reached only through " + path;
        changed = true;
      }
    });
  }
  st.lines = st.lines.filter(function (l) { return !st.dead[l.anchor]; });
}

// openLine pushes a line of reasoning anchored on a derived fact that the
// model judged relevant: a candidate answer whose members can still fail.
function openLine(path) {
  if (st.lines.length >= MAX_LINES) return;
  for (var i = 0; i < st.lines.length; i++) if (st.lines[i].anchor === path) return;
  st.lines.push({ anchor: path, opened_at: st.step });
}

// checkPremises runs after refs_out(f) for a derived, relevant fact f: a
// premise that is retracted at HEAD (explain marks it deleted), or one the
// model judged to contradict the question's answer, means the line f anchors
// rests on something we no longer believe. A premise that is merely
// superseded is history: its current version was read and judged instead.
function checkPremises(path) {
  var f = st.facts[path];
  if (!f || !alive(path) || !isDerived(f) || f.relevant !== "yes") return null;
  for (var i = 0; i < f.children.length; i++) {
    var c = f.children[i];
    if (c.deleted) return { from: path, premise: localPath(c.path), premiseTitle: c.title, cause: "retracted" };
  }
  for (var j = 0; j < f.children.length; j++) {
    var cp = localPath(f.children[j].path);
    var cf = st.facts[cp];
    if (cf && cf.verdict === "contradicts") return { from: path, premise: cp, premiseTitle: cf.title, cause: "contradicted" };
  }
  return null;
}

// --- states -----------------------------------------------------------------
var STATES = {
  GATHER: function () {
    var m = mv("neighbours", "", "question");
    st.moves++;
    st.done[moveKey(m)] = true;
    var q = knomit.query({ text: question, limit: QUERY_LIMIT, log: logOf("GATHER", m) });
    var rows = (q && q.facts) || [];
    rows.forEach(function (row) {
      var p = addFact(row, "question", moveKey(m));
      if (p) st.pending.push(p);
    });
    record("GATHER", m, rows.map(function (r) { return localPath(r.file); }), "", rows.length + " facts");
    if (!st.pending.length) {
      st.gaps.push({ question: question, posterior: 0, tried: st.tried.slice(), reason: "the world holds no facts near the question" });
      return "GAP";
    }
    return "ASSESS";
  },

  // ASSESS judges what the last read surfaced: relevance for every new fact,
  // then support for the relevant evidence (after reading it in full). A
  // retracted fact is history and is not judged; a relevant hypothesis adds
  // no evidence and queues a SEEK for what would settle it.
  ASSESS: function () {
    var pending = uniq(st.pending).filter(function (p) {
      var f = st.facts[p];
      return f && !st.dead[p] && !f.deleted && !f.relevant && roleOf(f) !== "excluded";
    });
    st.pending = [];
    if (pending.length) {
      var rel = judgeRelevance(pending, "ASSESS");
      // a fact the model found irrelevant does not bear on the answer: its
      // support verdict is `unrelated` without a second call
      var irrelevant = {};
      pending.forEach(function (p) {
        var f = st.facts[p];
        if (rel[p] === "no" && f.relevant === "no" && !f.verdict && (roleOf(f) === "evidence" || roleOf(f) === "premise")) {
          f.verdict = "unrelated";
          irrelevant[p] = "unrelated";
        }
      });
      if (Object.keys(irrelevant).length) {
        record("ASSESS", null, Object.keys(irrelevant), "support", "harness: not relevant, so unrelated", irrelevant);
      }
      var toJudge = [];
      pending.forEach(function (p) {
        if (rel[p] !== "yes") return;
        var f = hydrate(p, "ASSESS");
        if (f.deleted) return;
        var role = roleOf(f);
        if (role === "evidence" || role === "premise") toJudge.push(p);
        if (role === "open") st.seekQueue.push(f.title + " " + firstSentence(f.body));
        if (isDerived(f)) openLine(p);
        expand(f);
      });
      if (toJudge.length) judgeSupport(toJudge, "ASSESS");
    }
    if (st.premiseCheck) {
      var pb = checkPremises(st.premiseCheck);
      st.premiseCheck = "";
      if (pb) {
        st.pendingBacktrack = pb;
        return "BACKTRACK";
      }
    }
    return "WIDEN";
  },

  WIDEN: function () {
    if (st.final || st.moves >= MAX_MOVES) return "SYNTHESIZE";
    if (st.seekQueue.length && st.seeks < MAX_SEEKS) return "SEEK";
    var items = menu();
    if (!items.length) return "SYNTHESIZE";
    var e = chooseFrom("choose_move", "WIDEN", items);
    if (!e) return "SYNTHESIZE";
    execute(e, "WIDEN");
    return "ASSESS";
  },

  // BACKTRACK: the line anchored on pb.from rests on a dead premise. The
  // harness confirms it with supersession(from) — the fact re-read at HEAD,
  // its premises' flags fresh — pops the line, and the model picks the next
  // frontier entry from what is still alive.
  BACKTRACK: function () {
    var pb = st.pendingBacktrack;
    st.pendingBacktrack = null;
    var m = mv("supersession", pb.from);
    st.done[moveKey(m)] = true;
    var f = st.facts[pb.from];
    try {
      f.hydrated = false;
      hydrate(pb.from, "BACKTRACK");
    } catch (e) {
      if (isDeadline(e)) throw e;
    }
    var labels = {};
    labels[pb.premise] = "unrelated";
    labels[pb.from] = "unrelated";
    if (st.facts[pb.premise]) st.facts[pb.premise].verdict = "unrelated";
    f.verdict = "unrelated";
    abandon(pb.from, pb.cause + " premise " + pb.premise);
    st.abandoned.push(pb);
    st.backtracks++;
    st.posterior = posteriorOf();
    record("BACKTRACK", m, [pb.from, pb.premise], "", "line abandoned: its premise is " + pb.cause);
    // the harness's verdict, not a model's: what the line rested on is
    // history now, so neither it nor its anchor supports the answer
    record("BACKTRACK", null, [pb.from, pb.premise], "support", "harness: " + pb.cause + " premise; the line is history", labels);
    if (st.backtracks > MAX_BACKTRACKS) {
      st.final = true;
      return "SYNTHESIZE";
    }
    if (st.final || st.moves >= MAX_MOVES) return "SYNTHESIZE";
    var items = menu();
    if (!items.length) return "SYNTHESIZE";
    var e = chooseFrom("backtrack_choice", "BACKTRACK", items, [
      "Abandoned line: " + f.title + " (" + pb.from + ") rests on " + (pb.premiseTitle || pb.premise) + ", which is " + pb.cause + ".",
    ]);
    if (!e) return "SYNTHESIZE";
    execute(e, "BACKTRACK");
    return "ASSESS";
  },

  SYNTHESIZE: function () {
    var sup = supportPaths();
    st.posterior = posteriorOf();
    if (!sup.length) {
      st.draft = null;
      return "DECIDE";
    }
    // dedup, not append: one fact that already IS the answer is returned
    var restates = sup.filter(function (p) { return st.facts[p].verdict === "restates"; });
    if (restates.length === 1 && sup.length === 1) {
      st.existing = restates[0];
      st.posterior = Math.round(st.facts[restates[0]].confidence * 100) / 100;
      st.outcome = "answer";
      record("SYNTHESIZE", null, restates, "restates", "the answer exists: " + restates[0]);
      return "";
    }
    var lines = ["Question: " + question, "", "Supporting facts:"];
    sup.forEach(function (p, i) { lines.push(factLine(i + 1, st.facts[p], true)); });
    var con = contradictPaths();
    if (con.length) {
      lines.push("", "Facts that contradict (weigh them; do not build on them):");
      con.forEach(function (p) { lines.push("- " + st.facts[p].title); });
    }
    lines = lines.concat(guidance());
    var o = judge("synthesize", "SYNTHESIZE", mv(""), sup, lines.join("\n"), SYNTHESIZE_SCHEMA);
    if (!o || !String(o.claim || "").trim()) {
      st.draft = null;
      if (o) record("SYNTHESIZE", null, sup, "synthesize", "no claim");
      return "DECIDE";
    }
    st.draft = {
      claim: String(o.claim).trim(),
      consequence: String(o.consequence || "").trim(),
      misreading: String(o.misreading || "").trim(),
      carriers: uniq((o.carriers || []).map(function (i) { return sup[i - 1]; })),
    };
    record("SYNTHESIZE", null, sup, "synthesize", st.draft.claim.slice(0, 200));
    return "DECIDE";
  },

  // DECIDE: the model judges whether the draft answers the question; the
  // harness computes the posterior and applies the asker's trust bar.
  DECIDE: function () {
    var sup = supportPaths();
    st.posterior = posteriorOf();
    var verdict = "not",
      missing = "";
    if (st.draft) {
      var lines = ["Question: " + question, "", "Drafted claim: " + st.draft.claim, "", "Supporting facts:"];
      sup.forEach(function (p, i) { lines.push(factLine(i + 1, st.facts[p], false)); });
      lines = lines.concat(guidance());
      var o = judge("assess", "DECIDE", mv(""), sup, lines.join("\n"), ASSESS_SCHEMA);
      if (o) {
        verdict = o.assess;
        missing = String(o.missing || "").trim();
        record("DECIDE", null, sup, "assess", verdict + ", posterior " + st.posterior);
      }
    }
    st.assess = verdict;
    if (missing) st.missing = missing;
    var exhausted = st.final || st.seeks >= MAX_SEEKS || st.moves >= MAX_MOVES;
    if (st.draft && st.posterior >= TRUST && (verdict === "answers" || (verdict === "partly" && exhausted))) return "EMIT";
    if (!exhausted) return "HYPOTHESIZE";
    st.gaps.push({
      question: question,
      missing: st.missing || (st.hypothesis && st.hypothesis.claim) || "",
      best_partial: st.draft ? st.draft.claim : "",
      posterior: st.posterior,
      tried: st.tried.slice(),
      contradictions: contradictPaths(),
      reason: !sup.length ? "no fact supports an answer" : !st.draft ? "the model drew no claim from the support" :
        verdict !== "answers" && verdict !== "partly" ? "the support does not answer the question (" + verdict + ")" :
        "the posterior " + st.posterior + " is under the trust bar " + TRUST,
    });
    return "HYPOTHESIZE";
  },

  // HYPOTHESIZE: the missing premise as a falsifiable prediction with its
  // settlement criterion. On the way to a gap it becomes the gap's
  // hypothesis; otherwise SEEK looks for what would settle it.
  HYPOTHESIZE: function () {
    var lines = ["Question: " + question, ""];
    if (st.draft) lines.push("Best partial answer: " + st.draft.claim, "");
    if (st.missing) lines.push("What seems missing: " + st.missing, "");
    var rel = livePaths(function (f) { return f.relevant === "yes" && (roleOf(f) === "evidence" || roleOf(f) === "premise"); });
    lines.push("Relevant facts:");
    if (!rel.length) lines.push("(none)");
    rel.slice(0, JUDGE_BATCH).forEach(function (p, i) { lines.push(factLine(i + 1, st.facts[p], false)); });
    lines = lines.concat(movesTaken(), guidance());
    var o = judge("hypothesize", "HYPOTHESIZE", mv(""), rel.slice(0, 5), lines.join("\n"), HYPOTHESIZE_SCHEMA);
    if (o && String(o.claim || "").trim()) {
      st.hypothesis = { claim: String(o.claim).trim(), settlement: String(o.settlement || "").trim(), observable: String(o.observable || "").trim() };
      record("HYPOTHESIZE", null, [], "hypothesize", st.hypothesis.claim.slice(0, 200));
    } else if (!st.hypothesis) {
      st.hypothesis = {
        claim: st.missing || "The knowledge base holds the facts that answer: " + question,
        settlement: "A fact is written that states this claim with its own evidence.",
        observable: "",
      };
    }
    if (st.gaps.length) return "GAP";
    st.seekQueue.unshift(st.hypothesis.settlement || st.hypothesis.claim);
    return "SEEK";
  },

  // SEEK: the model phrases the settling evidence as the fact that would
  // exist if it did (a hypothetical fact to search by, E02 §9 seek); the
  // harness searches by it and judges what comes back.
  SEEK: function () {
    var target = st.seekQueue.shift() || (st.hypothesis && st.hypothesis.claim) || question;
    st.seeks++;
    var lines = ["Question: " + question, "", "Hypothesis or open question: " + target];
    var o = judge("seek", "SEEK", mv(""), [], lines.join("\n"), SEEK_SCHEMA);
    var text = o && String(o.search || "").trim() ? String(o.search).trim() : target;
    var m = mv("seek", "", text.slice(0, 160));
    st.moves++;
    var surfaced = [];
    try {
      var res = knomit.query({ text: text, limit: QUERY_LIMIT, log: logOf("SEEK", m) });
      ((res && res.facts) || []).forEach(function (row) {
        var p = addFact(row, "question", "seek");
        surfaced.push(localPath(row.file));
        if (p) st.pending.push(p);
      });
    } catch (e) {
      if (isDeadline(e)) throw e;
    }
    record("SEEK", m, surfaced, "", surfaced.length + " facts, " + st.pending.length + " new");
    return "ASSESS";
  },

  // EMIT: re-explain every support at HEAD before writing (the read set's
  // freshness check, E02 §7). A support retracted since it was read is
  // dropped and the answer re-decided; one rewritten since is re-judged on
  // its new version. Then write the answer fact into the run's experiment.
  EMIT: function () {
    var sup = supportPaths();
    var moved = [];
    sup.forEach(function (p) {
      var f = st.facts[p];
      var m = mv("freshness", p);
      var ex = null;
      try {
        ex = knomit.explain(p, { log: logOf("EMIT", m) });
      } catch (e) {
        if (isDeadline(e)) throw e;
      }
      var root = ((ex && ex.facts) || []).filter(function (n) { return n.depth === 0; })[0];
      if (!root || root.deleted) {
        f.deleted = true;
        moved.push(p);
      } else if (f.commit && root.commit && root.commit !== f.commit) {
        f.hydrated = false;
        f.relevant = "";
        f.verdict = "";
        absorbExplain(f, ex);
        moved.push(p);
        st.pending.push(p);
      }
    });
    if (moved.length) {
      record("EMIT", mv("freshness", "", moved.length + " moved"), moved, "", moved.length + " supports moved since they were read");
      st.freshness++;
      if (st.freshness <= MAX_FRESHNESS) {
        st.posterior = posteriorOf();
        return st.pending.length ? "ASSESS" : "SYNTHESIZE";
      }
      st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "the support kept moving under the run" });
      return "GAP";
    }
    var f = answerFact();
    if (!f) {
      st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "no supporting fact has a topic of this repo to file the answer under" });
      return "GAP";
    }
    if (!writeWithPlaces(f, placesFor(supportPaths()), "EMIT")) return "GAP";
    st.outcome = "answer";
    return "";
  },

  GAP: function () {
    var g = st.gaps[st.gaps.length - 1];
    if (!g) {
      g = { question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "the run ended without an answer" };
      st.gaps.push(g);
    }
    var h = st.hypothesis || {
      claim: g.missing || st.missing || "The knowledge base holds the facts that answer: " + question,
      settlement: "A fact is written that states this claim with its own evidence.",
      observable: "",
    };
    if (!g.missing) g.missing = h.claim;
    st.outcome = "gap";
    var partial = partialSupport();
    var places = placesFor(partial);
    if (!places.length) return ""; // nothing in this repo to file the hypothesis beside
    var body = [
      h.claim,
      "",
      "Open question: " + question,
      "",
      "Settles when: " + (h.settlement || "a fact is written that states this claim with its own evidence") + ".",
    ];
    if (h.observable) body.push("Observable: " + h.observable + ".");
    if (g.best_partial) body.push("", "Best partial answer (posterior " + g.posterior + "): " + g.best_partial);
    if (st.abandoned.length) body.push("", abandonedText());
    body.push(
      "",
      "Evidence-free by construction: it carries no corroboration until a fact that matches it is written. " +
        "Recorded by reasoning run " + run.id + " on " + world.repo + "@" + world.branch + ", reason: " + g.reason + "."
    );
    var f = {
      title: String(h.claim).slice(0, 200),
      body: body.join("\n"),
      kind: "epistemic",
      type: "hypothesis",
      confidence: Math.round(Math.max(0.1, Math.min(0.5, g.posterior || 0.3)) * 100) / 100,
      domain: rankedValues(partial, "domain", "", 6),
      entities: rankedValues(partial, "entities", h.claim, 8),
      refs: partial.slice(0, 8),
    };
    writeWithPlaces(f, places, "GAP");
    return "";
  },
};

// partialSupport is what a gap's hypothesis cites: the live supports, else the
// live relevant evidence. Never a hypothesis, a dead line, or a fact the
// model judged irrelevant.
function partialSupport() {
  var sup = supportPaths();
  if (sup.length) return sup;
  var rel = livePaths(function (f) { return f.relevant === "yes" && (roleOf(f) === "evidence" || roleOf(f) === "premise"); });
  if (rel.length) return rel;
  return st.order.filter(function (p) { return alive(p) && roleOf(st.facts[p]) === "evidence"; }).slice(0, 3);
}

// --- the answer fact ----------------------------------------------------------
function answerFact() {
  var d = st.draft;
  var sup = supportPaths();
  var indep = independentSupports();
  var meth = livePaths(function (f) { return roleOf(f) === "instructs" && f.relevant === "yes"; });
  var rules = livePaths(function (f) { return roleOf(f) === "constrains" && f.relevant === "yes"; });
  var con = contradictPaths();
  var title = function (p) { return st.facts[p].title + " (" + p + (st.facts[p].commit ? "@" + st.facts[p].commit.slice(0, 8) : "") + ")"; };
  var body = [d.claim, ""];
  if (d.consequence) body.push(d.consequence, "");
  if (d.misreading) body.push("What this does not mean: " + d.misreading, "");
  body.push("Answer to the question: " + question, "");
  body.push("Rests on (each read at HEAD of " + world.repo + "@" + world.branch + "):");
  sup.forEach(function (p) { body.push("- " + title(p)); });
  if (d.carriers.length) body.push("", "Conditions carried from: " + d.carriers.map(function (p) { return st.facts[p] ? st.facts[p].title : p; }).join("; ") + ".");
  if (con.length) body.push("", "Weighed against (and outweighed): " + con.map(title).join("; ") + ".");
  if (meth.length) body.push("", "Reasoned by: " + meth.map(title).join("; ") + ".");
  if (rules.length) body.push("", "Obeys: " + rules.map(title).join("; ") + ".");
  sup.forEach(function (p) {
    var h = st.facts[p].history;
    if (h) body.push("", "How this came to be believed: " + st.facts[p].title + " has " + h.revisions + " revisions; at " + h.commit.slice(0, 8) + " it read \"" + h.title + "\".");
  });
  if (st.abandoned.length) body.push("", abandonedText());
  body.push(
    "",
    "Confidence " + st.posterior + " is the harness's posterior: the weakest of " + indep.length +
      " independent supports" + (con.length ? ", against " + con.length + " contradicting" : "") + ". Reasoning run " + run.id + "."
  );
  var f = {
    title: String(d.claim).slice(0, 200),
    body: body.join("\n"),
    kind: "epistemic",
    type: "synthesis",
    origin: "authored", // omitted, a synthesis's origin means distilled
    confidence: st.posterior,
    sources: indep.length,
    domain: rankedValues(sup, "domain", "", 6),
    entities: rankedValues(sup, "entities", d.claim, 8),
    // refs are WHY we believe it: the supports, the methodology that shaped
    // the chain and the rules that constrained it. A contradicting fact is
    // not a reason to believe the answer; it is named in the body instead.
    refs: uniq(sup.concat(meth, rules)),
  };
  var motifs = sharedMotifs(indep);
  if (motifs.length) f.motifs = motifs;
  return f;
}

function abandonedText() {
  return "Abandoned lines: " + st.abandoned.map(function (a) {
    var f = st.facts[a.from];
    return (f ? f.title : a.from) + " (" + a.from + ") rests on " + (a.premiseTitle || a.premise) + ", which is " + a.cause +
      " at HEAD; it is history, not evidence";
  }).join("; ") + ".";
}

// rankedValues: the values of key across facts, the ones the claim names
// first, then by how many facts carry them.
function rankedValues(paths, key, claim, max) {
  var count = {},
    first = {},
    i = 0;
  paths.forEach(function (p) {
    (st.facts[p][key] || []).forEach(function (v) {
      if (!count[v]) first[v] = i++;
      count[v] = (count[v] || 0) + 1;
    });
  });
  var c = String(claim || "").toLowerCase();
  var vals = Object.keys(count);
  vals.sort(function (a, b) {
    var na = c && c.indexOf(a.toLowerCase()) >= 0 ? 1 : 0,
      nb = c && c.indexOf(b.toLowerCase()) >= 0 ? 1 : 0;
    if (na !== nb) return nb - na;
    if (count[a] !== count[b]) return count[b] - count[a];
    return first[a] - first[b];
  });
  return vals.slice(0, max);
}

// sharedMotifs: a motif the answer instantiates is one at least two of its
// independent supports carry.
function sharedMotifs(paths) {
  var count = {};
  paths.forEach(function (p) {
    uniq(st.facts[p].motifs).forEach(function (m) { count[m] = (count[m] || 0) + 1; });
  });
  return Object.keys(count).filter(function (m) { return count[m] >= 2; }).slice(0, 4);
}

// --- placement ----------------------------------------------------------------
// placeOf: topic and category of a path in THIS repo, under its ontology root
// (which may be more than one segment) — root/<topic>/<category...>/<id>.md.
// A path in another mount (kb://<other id>/...) or outside the root has no
// place here (fix-later 9).
function placeOf(path) {
  var p = localPath(path);
  if (p.indexOf("kb://") === 0) return null;
  var root = String(world.root || "kb").replace(/\/+$/, "");
  if (p.indexOf(root + "/") !== 0) return null;
  var segs = p.slice(root.length + 1).split("/");
  if (segs.length < 3) return null;
  var topic = segs[0].toLowerCase();
  var topics = (ontology && ontology.topics) || [];
  if (topics.length && topics.indexOf(topic) < 0) return null;
  return { topic: topic, category: segs.slice(1, segs.length - 1).join("/") };
}

// placesFor: candidate places for a fact resting on paths, best first.
// When the facts include a live derived fact (a synthesis or insight the
// answer refines), beside the earliest-read one; then the MAJORITY topic of
// the facts (ties to the earliest-read) with the majority category inside
// it; then each other topic the facts sit in. EMIT tries them in order when
// a topic's rules refuse the write. Over the pilot worlds' gold placements
// the derived-first rule matches 221 of 255 answer topics, the majority rule
// alone 216.
function placesFor(paths) {
  var out = [];
  var seen = {};
  var push = function (pl) {
    if (!pl) return;
    var k = pl.topic + "/" + pl.category;
    if (seen[k]) return;
    seen[k] = true;
    out.push(pl);
  };
  var ordered = uniq(paths).slice().sort(function (a, b) { return st.order.indexOf(a) - st.order.indexOf(b); });
  ordered.forEach(function (p) {
    if (st.facts[p] && isDerived(st.facts[p]) && alive(p)) push(placeOf(p));
  });
  majorityPlaces(paths).forEach(push);
  return out;
}

function majorityPlaces(paths) {
  var topicCount = {},
    catCount = {},
    order = [];
  uniq(paths).forEach(function (p) {
    var pl = placeOf(p);
    if (!pl) return;
    if (!topicCount[pl.topic]) order.push(pl.topic);
    topicCount[pl.topic] = (topicCount[pl.topic] || 0) + 1;
    var k = pl.topic + "\u0000" + pl.category;
    if (!catCount[k]) catCount[k] = { n: 0, at: Object.keys(catCount).length, place: pl };
    catCount[k].n++;
  });
  var topics = order.slice().sort(function (a, b) {
    if (topicCount[a] !== topicCount[b]) return topicCount[b] - topicCount[a];
    return order.indexOf(a) - order.indexOf(b);
  });
  return topics.map(function (t) {
    var best = null;
    for (var k in catCount) {
      var c = catCount[k];
      if (c.place.topic !== t) continue;
      if (!best || c.n > best.n || (c.n === best.n && c.at < best.at)) best = c;
    }
    return best.place;
  });
}

// contextFor puts the run key into `context` ONLY when the ontology declares
// reason_run on the fact's topic walk (the deepest declared prefix, as
// ContextSpec resolves it): a topic with no declaration refuses every key.
function contextFor(place) {
  var decl = (ontology && ontology.context) || {};
  var segs = (place.topic + "/" + place.category).toLowerCase().split("/");
  var keys = null,
    path = "";
  for (var i = 0; i < segs.length; i++) {
    path = path ? path + "/" + segs[i] : segs[i];
    if (decl[path]) keys = decl[path];
  }
  if (keys && keys.indexOf("reason_run") >= 0) return { reason_run: run.id };
  return null;
}

// writeWithPlaces writes f at the first place whose topic accepts it. The
// write path is the validator: a refusal there (a topic's rules, ValidateFact,
// the refs gate) moves on to the next place; when every place refuses, the
// fact is returned unwritten and the run records a gap naming why.
function writeWithPlaces(f, places, state) {
  if (!places.length) return false;
  if (!world.writable) {
    f.topic = places[0].topic;
    f.category = places[0].category;
    st.unwritten.push(f);
    return true;
  }
  if (!openExperiment(f, state)) return false;
  var reasons = [];
  for (var i = 0; i < places.length && i < 3; i++) {
    var g = {};
    for (var k in f) g[k] = f[k];
    g.topic = places[i].topic;
    g.category = places[i].category;
    var ctx = contextFor(places[i]);
    if (ctx) g.context = ctx;
    try {
      knomit.learn(g, { moment_name: "reason:" + run.id, log: logOf(state, mv("learn", "", g.type)) });
      record(state, mv("learn", "", g.type), [], "", "written under " + g.topic + "/" + g.category);
      return true;
    } catch (e) {
      if (isDeadline(e)) throw e;
      reasons.push(g.topic + "/" + g.category + ": " + errText(e));
      f = g;
    }
  }
  st.unwritten.push(f);
  st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "the write was refused: " + reasons.join(" | ") });
  return false;
}

function openExperiment(f, state) {
  try {
    knomit.experiment({
      action: "open",
      name: "reason-" + slug(question, 40) + "-" + run.id.slice(7),
      description: "knomit_reason " + run.id + ": " + question,
      log: logOf(state, mv("experiment", "", "open")),
    });
  } catch (e) {
    if (isDeadline(e)) throw e;
    if (errText(e).indexOf("already inside") < 0) {
      st.unwritten.push(f);
      st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "no experiment: " + errText(e) });
      return false;
    }
  }
  return true;
}

// --- the loop -----------------------------------------------------------------
// One state per step. The step budget bounds exploration: when it is spent
// the loop stops exploring and closes (synthesize, decide, then emit or gap),
// which the closing states always get to run; a hard ceiling past that ends
// it on a gap whatever state it is in.
var EXPLORING = { GATHER: 1, ASSESS: 1, WIDEN: 1, BACKTRACK: 1, HYPOTHESIZE: 1, SEEK: 1 };

function main() {
  var state = "GATHER";
  while (state) {
    if (st.step >= MAX_STEPS && !st.final) {
      st.final = true;
      if (EXPLORING[state]) state = "SYNTHESIZE";
    }
    if (st.step >= MAX_STEPS + 12 && state !== "GAP") {
      st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "the step budget ran out" });
      state = "GAP";
    }
    st.step++;
    state = STATES[state]();
  }
  var res = { outcome: st.outcome || "gap", posterior: st.posterior, chain: st.chain, gaps: st.outcome === "answer" ? [] : st.gaps };
  if (st.existing) res.existing_answer = st.existing;
  if (st.unwritten.length) res.unwritten_facts = st.unwritten;
  return res;
}

main();
