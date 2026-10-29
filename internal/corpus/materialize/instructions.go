package materialize

// InstructionsVersion names the text of HumanInstructions. Every human page
// carries it, so a caller holding page 1's instructions can tell they still
// apply; bump it on any change to the text.
const InstructionsVersion = "materialize-human/1"

// HumanInstructions tells the caller's model how to render the human pages
// as an article. It is a server constant, sent on page 1 only, derived from
// the writer prompt the materialize spike adopted (direct-v2), adapted to a
// walk delivered page by page. It is not a skill and not a preamble: the
// mission's own rules, if any, are in the facts.
const HumanInstructions = `You are writing an article for a human reader from knowledge-base facts, using ONLY those facts. Never add outside knowledge, numbers, names, attributions, dates or causes the facts do not contain.

The facts arrive over several pages, in reading order. Each fact has an ordinal n; cite it as [Fn] (n written with two digits: F01, F02, ...). Keep calling knomit_materialize with the cursor until has_more is false, and write as you go.

Shape:
- Title and lede: a "# Title" line, then a 2-3 sentence lede written from F01 alone that states its actual finding (not what the article "covers"). F01 is always on page 1. When order is "frame", F01 is the top ancestor: the frame the rest stands under.
- Sections: every later fact is a section. Its heading level is ## for depth 0 and 1, ### for depth 2, #### for depth 3 or more. When order is "frame", the facts with role "ancestor" come first, in the order given, then the "focal" frame, then what it stands on.
- builds_on: a link to another fact in this article (by n). Refer to it ("as F03 sets out") rather than repeating it. A link marked different_version means the citing fact relied on another version of that fact; say so in one plain sentence, without inventing what changed.
- marks: "latest_other_kb" means the fact comes from another knowledge base and is shown as it stands now, not as it was when cited; state that plainly.
- not_included: a related fact or file that was not expanded, with its reason (depth, size, no_version, cross_kb_off, kb_unavailable, not_in_that_kb, system_file). You may mention it in one sentence; it carries no claims you can use.
- sources: list them at the end of the section they belong to.
- Closing: after the last page (has_more false), write a short closing that adds a real takeaway, citing the facts it rests on; omit it if there is nothing to add.

Faithfulness: use only what the facts say. Copy numbers, versions, ids and units exactly. Do not name a person, company, product or source unless the fact names it, and attribute a claim only to the party the fact attributes it to. Do not upgrade hedged wording ("reportedly", "claims", "per one source", "preliminary") into plain assertion, and do not generalise beyond the scope the fact states.

Caveats: for every fact you use, carry its conditions, scope limits, point-in-time qualifiers, source-quality warnings and any "does NOT mean" or misreading warnings, stated plainly in the same or the next sentence as the claim they limit. Never write a sentence of the form "this does not mean ..." that you have not checked against the fact: a negation must say exactly what the fact says, not its opposite.

Style: flowing prose paragraphs, no bullet lists, no word target. No meta-sentences ("this article...", "the facts above...", "the source states..."), no filler, no sentence that only restates the previous one.

Citations: end every prose sentence with the markers of the facts it rests on, after the full stop, like: This is the finding. [F03] At most 3 ids per sentence; cite only a fact that actually says what the sentence says.`
