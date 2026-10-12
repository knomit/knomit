// reason.js — the reasoning engine's loop (expert spike, E02), PR 1 skeleton.
//
// The harness (this script) owns the loop's mechanics; the model owns every
// judgment inside it, through knomit.model with a closed schema. The world is
// the asker's binding; writes go into an experiment this script opens and
// never commits (accepting the answer is the asker's decision).
//
// Globals (frozen): question (string), run {id, budget_ms}, world {repo,
// repo_id, branch, writable}, ontology {context: {<topic path>: [declared
// context keys]}}. Host: knomit.query, explain, learn, incoming,
// experiment, model — each takes a `log` object ({step, state, move,
// judgment, posterior}) that the host copies into the run's log line.
//
// States: GATHER -> WIDEN -> ASSESS -> EMIT | GAP. PR 2 adds SYNTHESIZE,
// HYPOTHESIZE, SEEK and BACKTRACK; the machine below is a table of state
// functions over one explicit `st` object so they slot in without reshaping
// it (each state returns the name of the next).
//
// The completion value of this script (the last expression) is the result
// the host returns: {outcome (answer | gap), posterior, chain, gaps, existing_answer?,
// unwritten_facts?}.

// --- the type-semantics table (E02 §4) -------------------------------------
// role: evidence (weighed), premise (an a-priori definition: the epistemic
// TYPE principle only, never the principles/ topic), instructs (methodology:
// HOW, not evidence), constrains (policy), biases (heuristic), excluded
// (signal). weight scales the fact's prior; 0 means it never carries support.
var TYPES = {
  observation: { role: "evidence", weight: 1.0 },
  reference: { role: "evidence", weight: 1.0 },
  insight: { role: "evidence", weight: 1.0 },
  process: { role: "evidence", weight: 1.0 },
  concept: { role: "evidence", weight: 0.5 },
  pattern: { role: "evidence", weight: 0.5 },
  synthesis: { role: "evidence", weight: 1.0 },
  principle: { role: "premise", weight: 1.0 },
  hypothesis: { role: "open", weight: 0.0 },
  methodology: { role: "instructs", weight: 0.0 },
  policy: { role: "constrains", weight: 0.0 },
  heuristic: { role: "biases", weight: 0.0 },
  signal: { role: "excluded", weight: 0.0 },
};

// The trust bar is the asker's (a request property, E02 §5). Until E05 fits a
// default on scenario worlds and the asker can pass one, this is the
// placeholder default, stated here so it is not mistaken for a measured value.
var DEFAULT_TRUST = 0.5;

// How many gathered facts are widened and judged. A briefing-size bound for a
// small model (E02 §6), not a corpus property.
var WIDEN_TOP = 3;
var GATHER_LIMIT = 8;

var SUPPORT_SCHEMA = {
  type: "object",
  additionalProperties: false,
  required: ["claim", "verdicts", "missing"],
  properties: {
    claim: { type: "string" },
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
    missing: { type: "string" },
  },
};

var SUPPORT_SYSTEM =
  "You judge whether numbered facts from a knowledge base answer a question. " +
  "Use ONLY the facts given; never your own knowledge. " +
  "For each fact, give one verdict: supports (it is evidence for an answer), contradicts (it is evidence against " +
  "the answer you state), restates (it already IS the answer, word for word in substance), unrelated. " +
  "Then state the answer in `claim` as ONE sentence built only from the supporting facts, with every condition " +
  "they state kept in full; leave `claim` empty when the facts do not answer the question. " +
  "In `missing`, state as one claim what fact would be needed to answer it, or empty when nothing is missing. " +
  "Reply with JSON only.";

// --- state ------------------------------------------------------------------
var st = {
  step: 0,
  posterior: 0,
  read: {}, // path -> {fact, how}
  gathered: [], // evidence candidates, in gather order
  constraints: [], // pragmatic facts gathered (obeyed, never weighed)
  methodology: [],
  widened: [], // [{path, refs_out: [...], refs_in: [...]}]
  judged: null, // {claim, verdicts, missing}
  supports: [],
  contradicts: [],
  restates: [],
  chain: [],
  tried: [],
  gaps: [],
  existing: "",
  unwritten: [],
  outcome: "",
};

// A move is named as the scenario format's gold chain names one
// (GoldMove {move, from, arg}): from for a fact-anchored move, arg otherwise.
function mv(name, from, arg) {
  return { move: name, from: from || "", arg: arg || "" };
}

function moveText(m) {
  return m.move + (m.from ? "(" + m.from + ")" : m.arg ? "(" + m.arg + ")" : "");
}

function logOf(state, m, judgment) {
  var l = { step: st.step, state: state, move: m.move, from: m.from, arg: m.arg };
  if (judgment) l.judgment = judgment;
  l.posterior = st.posterior;
  return l;
}

function record(state, m, facts, judgment, verdict, support) {
  var e = { step: st.step, state: state, move: m.move, facts: (facts || []).slice(0, 5), posterior: st.posterior };
  if (m.from) e.from = m.from;
  if (m.arg) e.arg = m.arg;
  if (judgment) e.judgment = judgment;
  if (verdict) e.verdict = verdict;
  if (support) e.support = support;
  st.chain.push(e);
  st.tried.push(moveText(m));
}

function typeOf(f) {
  var t = f.type || (f.frontmatter && f.frontmatter.type) || "observation";
  return TYPES[t] ? t : "observation";
}

function kindOf(f) {
  return f.kind || (f.frontmatter && f.frontmatter.kind) || "epistemic";
}

function confidenceOf(f) {
  var c = f.confidence;
  if (c === undefined && f.frontmatter) c = f.frontmatter.confidence;
  return typeof c === "number" ? c : 0.5;
}

function sourcesOf(f) {
  var s = f.sources;
  if (s === undefined && f.frontmatter) s = f.frontmatter.sources;
  return typeof s === "number" && s > 0 ? s : 1;
}

function listOf(f, key) {
  var v = f[key];
  if (v === undefined && f.frontmatter) v = f.frontmatter[key];
  return Array.isArray(v) ? v : [];
}

// prior: confidence x sources x the type's weight; pragmatic facts carry no
// support mass whatever their type (they are obeyed, not weighed).
function priorOf(f) {
  if (kindOf(f) === "pragmatic") return 0;
  return confidenceOf(f) * sourcesOf(f) * TYPES[typeOf(f)].weight;
}

// posterior: the support set's mass, less the contradiction mass, in the
// SumProductNorm shape the corpus already uses for evidence weight.
function posteriorOf() {
  var s = 0,
    c = 0;
  st.supports.forEach(function (f) { s += priorOf(f); });
  st.contradicts.forEach(function (f) { c += priorOf(f); });
  var p = s / (s + c + 1);
  return Math.round(Math.max(0, Math.min(1, p)) * 100) / 100;
}

function pathOf(f) {
  return f.file || f.path;
}

// topic and category of a fact path kb/<topic>/<category...>/<id>.md
function placeOf(path) {
  var segs = String(path).split("/");
  if (segs.length < 4) return null;
  return { topic: segs[1], category: segs.slice(2, segs.length - 1).join("/") };
}

// The answer's place: the MAJORITY topic of the supporting facts (ties go to
// the earliest-ranked), and the category of the highest-ranked support in
// that topic. E02 leaves the choice open; this rule keeps the answer beside
// the evidence it rests on.
function placeFor(facts) {
  var count = {},
    best = null;
  facts.forEach(function (f) {
    var p = placeOf(pathOf(f));
    if (!p) return;
    count[p.topic] = (count[p.topic] || 0) + 1;
  });
  facts.forEach(function (f) {
    var p = placeOf(pathOf(f));
    if (!p) return;
    if (!best || count[p.topic] > count[best.topic]) best = p;
  });
  return best;
}

// contextFor puts the run key into `context` ONLY when the ontology declares
// reason_run on the fact's topic walk (the deepest declared prefix, as
// ContextSpec resolves it): a topic with no declaration refuses every key.
// Without it the fact carries no context and is found by its Knomit-Trace
// trailer (the run key) and its refs.
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

function slug(text, max) {
  var s = String(text).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (s.length > max) s = s.slice(0, max).replace(/-+$/, "");
  return s || "question";
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

// --- states -----------------------------------------------------------------
var STATES = {
  GATHER: function () {
    st.step++;
    var move = mv("neighbours", "", "question");
    var q = knomit.query({ text: question, limit: GATHER_LIMIT, log: logOf("GATHER", move) });
    var rows = (q && q.facts) || [];
    rows.forEach(function (f) {
      var p = pathOf(f);
      if (!p || st.read[p]) return;
      st.read[p] = { fact: f, how: moveText(move) };
      var t = typeOf(f);
      if (TYPES[t].role === "excluded") return;
      if (kindOf(f) === "pragmatic") {
        st.constraints.push(f);
      } else if (TYPES[t].role === "instructs") {
        st.methodology.push(f);
      } else {
        st.gathered.push(f);
      }
    });
    record("GATHER", move, rows.map(pathOf), "", rows.length + " facts");
    if (st.gathered.length === 0) {
      st.gaps.push({ question: question, posterior: 0, tried: st.tried.slice(), reason: "the world holds no facts near the question" });
      return "GAP";
    }
    return "WIDEN";
  },

  WIDEN: function () {
    // One widening step: what each top fact rests on (refs_out, read at the
    // version the fact pinned) and what rests on it (refs_in, "cited by").
    st.step++;
    st.gathered.slice(0, WIDEN_TOP).forEach(function (f) {
      var p = pathOf(f);
      var ex = knomit.explain(p, { log: logOf("WIDEN", mv("refs_out", p)) });
      var nodes = (ex && ex.facts) || [];
      var root = null,
        out = [];
      nodes.forEach(function (n) {
        if (n.depth === 0 && n.path === p) root = n;
        else if (n.path) out.push(n.path);
      });
      if (root) {
        // the full body and fields replace the query's snippet
        for (var k in root) f[k] = root[k];
      }
      var inc = knomit.incoming(p, { log: logOf("WIDEN", mv("refs_in", p)) });
      var ins = ((inc && inc.refs) || []).map(function (r) { return r.path; });
      st.widened.push({ path: p, refs_out: out, refs_in: ins });
      record("WIDEN", mv("refs_out", p), out, "", out.length + " refs out");
      record("WIDEN", mv("refs_in", p), ins, "", ins.length + " citers");
    });
    return "ASSESS";
  },

  ASSESS: function () {
    st.step++;
    var facts = st.gathered.slice(0, WIDEN_TOP);
    var lines = ["Question: " + question, "", "Facts:"];
    facts.forEach(function (f, i) {
      lines.push(
        "[" + (i + 1) + "] (" + kindOf(f) + "/" + typeOf(f) + ", confidence " + confidenceOf(f) + ") " +
          (f.title || pathOf(f)) + "\n" + String(f.body || "").slice(0, 1500)
      );
    });
    if (st.constraints.length) {
      lines.push("", "Rules that constrain the answer (obey; they are not evidence):");
      st.constraints.slice(0, 3).forEach(function (f) { lines.push("- " + f.title); });
    }
    var r = knomit.model({
      schema: SUPPORT_SCHEMA,
      system: SUPPORT_SYSTEM,
      user: lines.join("\n"),
      log: logOf("ASSESS", mv("judge", "", facts.length + " facts"), "support"),
    });
    var j = r.output;
    st.judged = j;
    var support = {};
    (j.verdicts || []).forEach(function (v) {
      var f = facts[v.fact - 1];
      if (!f) return; // an index outside the menu is ignored, never guessed
      support[pathOf(f)] = v.verdict;
      if (v.verdict === "supports") st.supports.push(f);
      else if (v.verdict === "contradicts") st.contradicts.push(f);
      else if (v.verdict === "restates") st.restates.push(f);
    });
    st.posterior = posteriorOf();
    var verdict = st.supports.length + " supports, " + st.contradicts.length + " contradicts, " + st.restates.length + " restates";
    record("ASSESS", mv("judge", "", facts.length + " facts"), facts.map(pathOf), "support", verdict, support);

    // dedup, not append: a fact that already IS the answer is returned, and
    // nothing is written.
    if (st.restates.length === 1 && st.supports.length === 0 && st.contradicts.length === 0) {
      st.existing = pathOf(st.restates[0]);
      st.posterior = Math.round(confidenceOf(st.restates[0]) * 100) / 100;
      st.outcome = "answer";
      return "";
    }
    if (j.claim && st.supports.length > 0 && st.posterior >= DEFAULT_TRUST) return "EMIT";
    st.gaps.push({
      question: question,
      missing: j.missing || "",
      best_partial: j.claim || "",
      posterior: st.posterior,
      tried: st.tried.slice(),
      contradictions: st.contradicts.map(pathOf),
      reason: !j.claim ? "the model found no answer in the facts" :
        st.supports.length === 0 ? "no fact supports the claim" : "the posterior is under the trust bar " + DEFAULT_TRUST,
    });
    return "GAP";
  },

  EMIT: function () {
    st.step++;
    var j = st.judged;
    var place = placeFor(st.supports);
    if (!place) {
      st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "no supporting fact has a topic to file the answer under" });
      return "GAP";
    }
    var type = st.supports.length >= 2 ? "synthesis" : "insight";
    var refs = uniq(st.supports.map(pathOf).concat(st.contradicts.map(pathOf)));
    var body = [
      j.claim,
      "",
      "Answer to the question: " + question,
      "",
      "Supported by: " + st.supports.map(function (f) { return f.title + " (" + pathOf(f) + ")"; }).join("; ") + ".",
    ];
    if (st.contradicts.length) {
      body.push("Weighed against: " + st.contradicts.map(function (f) { return f.title + " (" + pathOf(f) + ")"; }).join("; ") + ".");
    }
    body.push("", "Confidence is the harness's posterior over the support set (" + st.posterior + "), reasoning run " + run.id + ".");
    var f = {
      topic: place.topic,
      category: place.category,
      title: String(j.claim).slice(0, 200),
      body: body.join("\n"),
      kind: "epistemic",
      type: type,
      confidence: st.posterior,
      sources: st.supports.length,
      domain: uniq([].concat.apply([], st.supports.map(function (x) { return listOf(x, "domain"); }))).slice(0, 8),
      entities: uniq([].concat.apply([], st.supports.map(function (x) { return listOf(x, "entities"); }))).slice(0, 8),
      refs: refs,
    };
    var ctxEmit = contextFor(place);
    if (ctxEmit) f.context = ctxEmit;
    if (type === "synthesis") f.origin = "authored";
    if (!write(f, "EMIT")) return "GAP";
    st.outcome = "answer";
    return "";
  },

  GAP: function () {
    st.step++;
    var g = st.gaps[st.gaps.length - 1] || { question: question, posterior: st.posterior, tried: st.tried.slice() };
    var place = placeFor(st.supports.length ? st.supports : st.gathered.slice(0, WIDEN_TOP));
    st.outcome = "gap";
    if (!place) return ""; // nothing in the world to file the hypothesis under
    var claim = g.missing || "The knowledge base holds the facts that answer: " + question;
    var f = {
      topic: place.topic,
      category: place.category,
      title: String(claim).slice(0, 200),
      body: [
        claim,
        "",
        "Open question: " + question,
        "",
        "Settles when: a fact is written that states this claim with its own evidence. Until then it carries no " +
          "corroboration. Recorded by reasoning run " + run.id + ", which reached posterior " + g.posterior + ".",
      ].join("\n"),
      kind: "epistemic",
      type: "hypothesis",
      confidence: 0.3,
      refs: uniq(st.supports.map(pathOf).concat(st.gathered.slice(0, WIDEN_TOP).map(pathOf))),
    };
    var ctxGap = contextFor(place);
    if (ctxGap) f.context = ctxGap;
    write(f, "GAP");
    return "";
  },
};

// write opens the run's experiment if needed and learns one fact there. A
// world where no experiment can be opened, or a refused write, gets the fact
// back unwritten.
function write(f, state) {
  if (!world.writable) {
    st.unwritten.push(f);
    return true;
  }
  try {
    knomit.experiment({ action: "open", name: "reason-" + slug(question, 40) + "-" + run.id.slice(7), description: "knomit_reason " + run.id + ": " + question, log: logOf(state, mv("experiment", "", "open")) });
  } catch (e) {
    if (String(e.message).indexOf("already inside") < 0) {
      st.unwritten.push(f);
      st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "no experiment: " + e.message });
      return false;
    }
  }
  var opts = { moment_name: "reason:" + run.id, log: logOf(state, mv("learn", "", f.type)) };
  try {
    knomit.learn(f, opts);
  } catch (e) {
    // The write path's own refusal (ValidateFact, the topic's rules, dedup)
    // is the validation: the fact is returned unwritten and the run ends on
    // a gap naming why; the host has logged the refusal at Warn.
    st.unwritten.push(f);
    st.gaps.push({ question: question, posterior: st.posterior, tried: st.tried.slice(), reason: "the write was refused: " + e.message });
    return false;
  }
  record(state, mv("learn", "", f.type), [], "", "written");
  return true;
}

function main() {
  var state = "GATHER";
  while (state) state = STATES[state]();
  var res = { outcome: st.outcome || "gap", posterior: st.posterior, chain: st.chain, gaps: st.gaps };
  if (st.existing) res.existing_answer = st.existing;
  if (st.unwritten.length) res.unwritten_facts = st.unwritten;
  return res;
}

main();
