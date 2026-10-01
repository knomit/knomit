// Pattern 1: claim / wait / take. A heuristic plus a backstop, never exclusion
// (README "Two ways to take" and "The timing rule").
//
// One script, three triggers (.knomit/ontology.yaml):
//   offer     on learn of tasks/**                  -> claim the task
//   decide    on due   of claims/*/{agent}/**       -> my claim's window is over
//   dup-check on learn of inbox/**                   -> two copies of one task
//
// Paths (under the ontology root, usually kb/):
//   tasks/<lane>/<id>.md                      one entity: the task id
//   claims/<task-id>/<agent-id>/<id>.md       expires = the claim's timer
//   inbox/<agent-id>/working/<id>.md          the queue: taken, not started
//   inbox/<agent-id>/active/<id>.md           taken by a session (work-task skill)
//
// Every machine ranks claimers the same way (rank below), so every machine
// that holds the same claims picks the same winner. The winner takes with the
// atomic move (knomit.learn with retract): the working copy is written and the
// task and the winner's claims are deleted in ONE commit, or nothing is.

// X, the claim window, in seconds (README "The timing rule"). With the
// ontology's `sync: {push: realtime, pull: realtime}` a claim reaches every
// decider within about 8 s: 3 s pull interval + 1 s push countdown + ~1 s for
// the host's merge and fast-forward + up to 3 s for one due tick. X = 15 s is
// almost 2x that. It holds only while every participant's origin is healthy;
// the re-arm and the dup-check cover a slow one. Without `sync` (or on a
// forge) X must be raised (README). The dead-winner threshold is 2X.
var WINDOW_SECONDS = 15;

// The BACKLOG: how many copies (working + active) this machine holds before
// it stops claiming, and before decide stops taking. How many sessions run
// at once is the recipe's `concurrent` (README "Capacity and parallelism").
var CAPACITY = 2;

// A working copy's lease, in seconds (README "The queue"): if no session has
// taken the copy by then, the `lease` trigger wakes one. It is never an
// expiry: nothing retracts a copy because its lease ran out.
var LEASE_SECONDS = 300;

// ---- helpers

// The ontology root (usually "kb"), from the firing path.
function root() {
  return change.path.split("/")[0];
}

// A ref to a fact in this repo is stored as kb://<repo-id>/<path>; this is
// the <path>.
function localPath(ref) {
  return String(ref).replace(/^kb:\/\/[^\/]+\//, "");
}

// RFC 3339, whole seconds. Date is the run's clock, the same one the due
// sweep compares against.
function iso(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

// Every fact under a path prefix (optionally carrying all of entities).
function under(prefix, entities) {
  var args = {path: prefix, limit: 100};
  if (entities) {
    args.entities = entities;
  }
  var r = knomit.query(args);
  return (r && r.facts) || [];
}

function exists(path) {
  return under(path).some(function (f) { return f.file === path; });
}

// One fact with its full body (title, body).
function read(path) {
  var r = knomit.query({path: path, include_body: true, limit: 1});
  return r.facts[0];
}

// Path segment i of a fact path (0 is the ontology root).
function seg(path, i) {
  return path.split("/")[i];
}

// ---- the rank: identical on every machine

// FNV-1a, 32 bit: deterministic in every JavaScript engine.
function fnv1a(s) {
  var h = 0x811c9dc5;
  for (var i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}

// ranksBefore says whether claimer a ranks ahead of claimer b for task. The
// hash spreads wins across machines; equal hashes fall back to the agent ids
// as strings, so the order is total and the same everywhere.
function ranksBefore(task, a, b) {
  var ha = fnv1a(task + "|" + a);
  var hb = fnv1a(task + "|" + b);
  if (ha !== hb) {
    return ha < hb;
  }
  return a < b;
}

// The first of a list of claimer ids.
function firstOf(task, ids) {
  var best = null;
  ids.forEach(function (id) {
    if (best === null || ranksBefore(task, id, best)) {
      best = id;
    }
  });
  return best;
}

function unique(list) {
  var seen = {};
  return list.filter(function (x) {
    if (seen[x]) {
      return false;
    }
    seen[x] = true;
    return true;
  });
}

// Every copy this machine holds: the queue and the copies sessions took.
function held() {
  return under(root() + "/inbox/" + agent.id + "/").length;
}

// Copies of one task anywhere in the inbox (working or active), any holder.
function copiesOf(task) {
  return under(root() + "/inbox/", [task]).filter(function (f) {
    var s = seg(f.file, 3);
    return s === "working" || s === "active";
  });
}

// ---- offer: a new task appeared; claim it

function offer() {
  var task = fact.entities[0];
  if (!task) {
    return;
  }
  var r = root();
  if (held() >= CAPACITY) {
    return;
  }
  if (under(r + "/claims/" + task + "/" + agent.id + "/").length > 0) {
    return; // already claimed by me
  }
  knomit.learn({
    topic: "claims",
    category: task + "/" + agent.id,
    kind: "pragmatic",
    type: "signal",
    title: "Claim of " + task + " by " + agent.id,
    body: "claimer: " + agent.id + "\ntask: " + change.path + "\n",
    entities: [task],
    refs: [change.path],
    expires: iso(Date.now() + WINDOW_SECONDS * 1000),
    confidence: 1,
    sources: 1
  });
}

// ---- decide: my claim's window is over

function decide() {
  var r = root();
  var mine = change.path;
  var task = fact.entities[0];
  var taskPath = localPath(fact.refs[0]);

  // 1. The task is gone (taken, expired) or somebody already works on it:
  //    withdraw my claim.
  var holders = copiesOf(task);
  if (!exists(taskPath) || holders.length > 0) {
    knomit.retract(mine);
    return;
  }

  // 2. Every claim for the task. A claim whose expires is more than 2X in
  //    the past belongs to a claimer presumed dead: it no longer ranks.
  var now = Date.now();
  var claims = under(r + "/claims/" + task + "/").map(function (f) {
    return {path: f.file, id: seg(f.file, 3), expires: Date.parse(f.frontmatter.expires)};
  });
  var live = claims.filter(function (c) {
    return !(now - c.expires > 2 * WINDOW_SECONDS * 1000);
  });
  var dead = claims.filter(function (c) {
    return now - c.expires > 2 * WINDOW_SECONDS * 1000;
  });

  // 3. The first live claimer, by rank.
  var winner = firstOf(task, unique(live.map(function (c) { return c.id; })));

  // 4. Not first: wait one more window. Re-arming expires re-arms this
  //    trigger. If the winner takes, step 1 withdraws my claim at my next
  //    decide; if it never does, its claim ages past 2X and I become first.
  if (winner !== agent.id) {
    knomit.update(mine, {updates: {expires: iso(now + WINDOW_SECONDS * 1000)}});
    return;
  }

  // 5. First, but full: the backlog filled up since I claimed (assigned
  //    work, another take). Withdraw; the next claimer by rank takes at its
  //    next decide.
  if (held() >= CAPACITY) {
    knomit.retract(mine);
    return;
  }

  // 6. First: take. One commit writes my working copy (with its lease) and
  //    deletes the task, my own claims and any dead claims, or none of it
  //    happens.
  var retract = [taskPath];
  claims.forEach(function (c) {
    if (c.id === agent.id || dead.indexOf(c) >= 0) {
      retract.push(c.path);
    }
  });
  var src = read(taskPath);
  try {
    knomit.learn({
      topic: "inbox",
      category: agent.id + "/working",
      kind: "pragmatic",
      type: "signal",
      title: src.title,
      body: src.body + "\n\ntask: " + taskPath + "\ntaker: " + agent.id + "\n",
      entities: [task],
      expires: iso(now + LEASE_SECONDS * 1000),
      confidence: 1,
      sources: 1
    }, {retract: unique(retract)});
  } catch (e) {
    // Nothing was written. If the task vanished between step 1 and the
    // move, withdraw. Any other failure is rethrown: the fire is logged as a
    // script error, and my claim stays; if I never take, it ages past 2X and
    // the next claimer takes instead (the re-offer).
    if (exists(taskPath)) {
      throw e;
    }
    knomit.retract(mine);
  }
}

// ---- dup-check: two copies of one task (the backstop)

function dupCheck() {
  var task = fact.entities[0];
  var copies = copiesOf(task);
  var mine = copies.filter(function (f) { return seg(f.file, 2) === agent.id; });
  if (copies.length < 2 || mine.length === 0) {
    return;
  }
  var keeper = firstOf(task, unique(copies.map(function (f) { return seg(f.file, 2); })));
  if (keeper === agent.id) {
    return;
  }
  // The other holder ranks first: back off. Only my own copies are touched.
  knomit.learn([], {retract: mine.map(function (f) { return f.file; })});
}

// ---- dispatch on the firing path's topic

if (typeof change !== "undefined" && change) {
  var topic = seg(change.path, 1);
  if (topic === "claims" && change.episode === "due") {
    decide();
  } else if (topic === "inbox") {
    dupCheck();
  } else if (topic === "tasks") {
    offer();
  }
}
