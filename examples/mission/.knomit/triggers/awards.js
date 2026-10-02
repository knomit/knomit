// Pattern 2: host-awarded, exactly once (README "Two ways to take").
//
// One script, three triggers (.knomit/ontology.yaml):
//   bid        on learn of offers/**            -> ask for the offer
//   award      on learn of bids/{agent}/**      -> runs on the AWARDER only
//   take-award on learn of awards/{agent}/**    -> runs on the winner only
//
// Paths (under the ontology root, usually kb/):
//   offers/<awarder-agent-id>/<lane>/<id>.md                one entity: the task id
//   bids/<awarder-agent-id>/<task-id>/<bidder-agent-id>/<id>.md
//   awards/<winner-agent-id>/<id>.md
//   inbox/<agent-id>/working/<id>.md                         the queue (README "The queue")
//
// Why exactly once: only the awarder writes awards (the award trigger's match
// names {agent}, so it fires on that one instance), and each award is the
// atomic move "write the award, delete the offer". A second award for the
// same offer finds the offer gone and is refused as a whole, under the
// awarder's own branch lock. The price is one more round trip, and the
// awarder must be up (normally it is the instance that hosts this repo).

// How long a bid waits for an award before its owner withdraws it
// (the withdraw-bid trigger, inline in the ontology).
var BID_SECONDS = 600;

// The BACKLOG: how many copies (working + active) this machine holds before
// it stops bidding. An award that arrives over it is still taken: the award
// already deleted the offer, so refusing it would lose the task. The extra
// copy waits in the queue (README "Capacity and parallelism").
var CAPACITY = 2;

// A working copy's lease, in seconds: the same rule as claims.js.
var LEASE_SECONDS = 300;

function root() {
  return change.path.split("/")[0];
}

function localPath(ref) {
  return String(ref).replace(/^kb:\/\/[^\/]+\//, "");
}

function iso(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function under(prefix, entities) {
  var args = {path: prefix, limit: 100};
  if (entities) {
    args.entities = entities;
  }
  var r = knomit.query(args);
  return (r && r.facts) || [];
}

function read(path) {
  var r = knomit.query({path: path, include_body: true, limit: 1});
  return r.facts[0];
}

function seg(path, i) {
  return path.split("/")[i];
}

// ---- bid: an offer appeared; ask for it

function bid() {
  var r = root();
  var awarder = seg(change.path, 2);
  var task = fact.entities[0];
  if (!task) {
    return;
  }
  if (under(r + "/inbox/" + agent.id + "/").length >= CAPACITY) {
    return;
  }
  if (under(r + "/bids/" + awarder + "/" + task + "/" + agent.id + "/").length > 0) {
    return; // already bid
  }
  knomit.learn({
    topic: "bids",
    category: awarder + "/" + task + "/" + agent.id,
    kind: "pragmatic",
    type: "signal",
    title: "Bid for " + task + " by " + agent.id,
    body: "bidder: " + agent.id + "\noffer: " + change.path + "\n",
    entities: [task],
    refs: [change.path],
    expires: iso(Date.now() + BID_SECONDS * 1000),
    confidence: 1,
    sources: 1
  });
}

// ---- award: a bid reached the awarder (this instance)

function award() {
  var bidder = seg(change.path, 4);
  var task = fact.entities[0];
  var offerPath = localPath(fact.refs[0]);
  var offer = read(offerPath);
  if (!offer) {
    return; // already awarded or expired; the bid is withdrawn at its expiry
  }
  try {
    knomit.learn({
      topic: "awards",
      category: bidder,
      kind: "pragmatic",
      type: "signal",
      title: offer.title,
      body: offer.body + "\n\noffer: " + offerPath + "\nwinner: " + bidder + "\n",
      entities: [task],
      confidence: 1,
      sources: 1
    }, {retract: [offerPath]});
  } catch (e) {
    // Another bid won in between: the offer is gone and nothing was written.
  }
}

// ---- take-award: I won; move the award into my working queue (never
// refused for capacity: see CAPACITY)

function takeAward() {
  var r = root();
  var task = fact.entities[0];
  var mine = under(r + "/bids/", [task]).filter(function (f) {
    return seg(f.file, 4) === agent.id;
  }).map(function (f) { return f.file; });
  var won = read(change.path);
  knomit.learn({
    topic: "inbox",
    category: agent.id + "/working",
    kind: "pragmatic",
    type: "signal",
    title: won.title,
    body: won.body,
    entities: [task],
    expires: iso(Date.now() + LEASE_SECONDS * 1000),
    confidence: 1,
    sources: 1
  }, {retract: [change.path].concat(mine)});
}

if (typeof change !== "undefined" && change) {
  var topic = seg(change.path, 1);
  if (topic === "offers") {
    bid();
  } else if (topic === "bids") {
    award();
  } else if (topic === "awards") {
    takeAward();
  }
}
